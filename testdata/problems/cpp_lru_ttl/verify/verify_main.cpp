#include "lru_cache.hpp"

#include <chrono>
#include <cstdint>
#include <cstdlib>
#include <iostream>
#include <string>
#include <vector>

#define VERIFY(cond)                                                               \
    do {                                                                           \
        if (!(cond)) {                                                             \
            std::cerr << "VERIFY FAILED [" << __FILE__ << ":" << __LINE__ << "]: " \
                      << #cond << std::endl;                                       \
            std::exit(1);                                                          \
        }                                                                          \
    } while (0)

namespace {

struct Record {
    int id = 0;
    std::string label;
    std::vector<int> values;

    bool operator==(const Record& other) const = default;
};

void test_basic_lru_and_recency() {
    TTLLRUCache<int, std::string> cache(3);
    VERIFY(cache.capacity() == 3);
    VERIFY(cache.size(0) == 0);

    cache.put(1, "one", 1000, 10);
    cache.put(2, "two", 1000, 20);
    cache.put(3, "three", 1000, 30);
    VERIFY(cache.size(30) == 3);

    // Touch key 1 and key 2 so key 3 becomes LRU
    VERIFY(cache.get(1, 40) == std::optional<std::string>("one"));
    VERIFY(cache.get(2, 45) == std::optional<std::string>("two"));

    // Insert key 4 -> should evict key 3 (LRU)
    cache.put(4, "four", 1000, 50);
    VERIFY(cache.size(50) == 3);
    VERIFY(!cache.get(3, 50).has_value());
    VERIFY(cache.get(1, 50) == std::optional<std::string>("one"));
    VERIFY(cache.get(2, 50) == std::optional<std::string>("two"));
    VERIFY(cache.get(4, 50) == std::optional<std::string>("four"));
}

void test_ttl_boundary_and_overwrite() {
    TTLLRUCache<std::string, int> cache(2);

    cache.put("k1", 100, 250, 1000); // expires at 1250
    VERIFY(cache.get("k1", 1249) == std::optional<int>(100));
    // Exact boundary: now_ms == expiry_ms is expired
    VERIFY(!cache.get("k1", 1250).has_value());
    VERIFY(cache.size(1250) == 0);
    VERIFY(!cache.erase("k1"));

    // Overwrite extending TTL and promoting recency
    cache.put("a", 1, 100, 2000);   // expires at 2100
    cache.put("b", 2, 500, 2010);   // expires at 2510
    cache.put("a", 11, 600, 2050);  // now expires at 2650, promoted to MRU ("b" is LRU)

    cache.put("c", 3, 500, 2060);   // expires at 2560, evicts "b" (LRU)
    VERIFY(!cache.get("b", 2060).has_value());
    VERIFY(cache.get("a", 2400) == std::optional<int>(11));

    // Overwrite shortening TTL
    cache.put("a", 12, 20, 2400);   // now expires at 2420
    VERIFY(cache.get("a", 2419) == std::optional<int>(12));
    VERIFY(!cache.get("a", 2420).has_value());
    VERIFY(cache.size(2500) == 1);  // only "c" remains live until 2560
    VERIFY(cache.size(2560) == 0);
}

void test_expired_evicted_before_live_lru() {
    TTLLRUCache<std::string, int> cache(2);

    // "long_lived" is inserted first -> becomes LRU, expires at 10000
    cache.put("long_lived", 1, 10000, 0);
    // "short_lived" is inserted second -> becomes MRU, expires at 50
    cache.put("short_lived", 2, 50, 0);
    VERIFY(cache.get("short_lived", 10) == std::optional<int>(2)); // still MRU

    // At t = 60, "short_lived" is expired while "long_lived" is still alive!
    // Inserting "fresh" must evict expired "short_lived", NOT live "long_lived".
    cache.put("fresh", 3, 500, 60);
    VERIFY(cache.size(60) == 2);
    VERIFY(cache.get("long_lived", 60) == std::optional<int>(1));
    VERIFY(cache.get("fresh", 60) == std::optional<int>(3));
    VERIFY(!cache.get("short_lived", 60).has_value());
}

void test_zero_and_unit_capacity_and_nonpositive_ttl() {
    TTLLRUCache<int, int> zero_cache(0);
    VERIFY(zero_cache.capacity() == 0);
    zero_cache.put(1, 42, 1000, 0);
    VERIFY(!zero_cache.get(1, 0).has_value());
    VERIFY(zero_cache.size(0) == 0);
    VERIFY(!zero_cache.erase(1));
    VERIFY(zero_cache.purge_expired(100) == 0);

    TTLLRUCache<int, int> unit_cache(1);
    unit_cache.put(10, 100, 50, 0);
    VERIFY(unit_cache.get(10, 25) == std::optional<int>(100));
    unit_cache.put(20, 200, 50, 30); // evicts 10
    VERIFY(!unit_cache.get(10, 30).has_value());
    VERIFY(unit_cache.get(20, 30) == std::optional<int>(200));

    // Overwrite with ttl_ms <= 0 removes existing key
    unit_cache.put(20, 999, 0, 35);
    VERIFY(!unit_cache.get(20, 35).has_value());
    VERIFY(unit_cache.size(35) == 0);

    unit_cache.put(30, 300, -10, 40);
    VERIFY(unit_cache.size(40) == 0);
}

void test_purge_expired_erase_and_struct_values() {
    TTLLRUCache<std::string, Record> cache(10);
    cache.put("r1", Record{1, "first", {1, 2}}, 50, 100);   // exp 150
    cache.put("r2", Record{2, "second", {3, 4}}, 100, 100); // exp 200
    cache.put("r3", Record{3, "third", {5, 6}}, 150, 100);  // exp 250
    cache.put("r4", Record{4, "fourth", {7, 8}}, 300, 100); // exp 400

    // At t = 200, r1 (150) and r2 (200) are expired; r3 (250) and r4 (400) are live
    size_t purged = cache.purge_expired(200);
    VERIFY(purged == 2);
    VERIFY(cache.purge_expired(200) == 0);
    VERIFY(cache.size(200) == 2);

    auto r3 = cache.get("r3", 210);
    VERIFY(r3.has_value());
    VERIFY((r3.value() == Record{3, "third", {5, 6}}));

    VERIFY(cache.erase("r3"));
    VERIFY(!cache.erase("r3"));
    VERIFY(!cache.get("r3", 210).has_value());
    VERIFY(cache.size(210) == 1);
}

void test_scale_o1_performance() {
    constexpr size_t kCap = 4096;
    constexpr int kOps = 50000;
    TTLLRUCache<int, int64_t> cache(kCap);

    auto t0 = std::chrono::steady_clock::now();
    int64_t now = 1000;
    int hits = 0;

    for (int i = 0; i < kOps; ++i) {
        now += 1;
        uint32_t ui = static_cast<uint32_t>(i);
        int key = static_cast<int>((ui * 2654435761u) & 8191u);
        int64_t ttl = 200 + (i % 5000);
        cache.put(key, static_cast<int64_t>(i) * 3, ttl, now);

        uint32_t up = static_cast<uint32_t>(i + 8175);
        int probe_key = static_cast<int>((up * 2654435761u) & 8191u);
        if (auto val = cache.get(probe_key, now); val.has_value()) {
            hits += (*val >= 0) ? 1 : 0;
        }
        if ((i & 4095) == 4095) {
            VERIFY(cache.size(now) <= kCap);
        }
    }

    auto t1 = std::chrono::steady_clock::now();
    auto elapsed_ms =
        std::chrono::duration_cast<std::chrono::milliseconds>(t1 - t0).count();

    VERIFY(hits > 0);
    VERIFY(cache.size(now) <= kCap);
    VERIFY(elapsed_ms < 500);
}

} // namespace

int main() {
    test_basic_lru_and_recency();
    test_ttl_boundary_and_overwrite();
    test_expired_evicted_before_live_lru();
    test_zero_and_unit_capacity_and_nonpositive_ttl();
    test_purge_expired_erase_and_struct_values();
    test_scale_o1_performance();

    std::cout << "All cpp_lru_ttl verification checks passed.\n";
    return 0;
}
