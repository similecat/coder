#pragma once

#include <cstddef>
#include <cstdint>
#include <optional>
#include <utility>

// C++20 LRU Cache with per-entry TTL expiration.
//
// Time semantics:
//   An item inserted via put(key, value, ttl_ms, now_ms) expires at:
//       expiry_ms = now_ms + ttl_ms
//   It is considered EXPIRED at timestamp `t` whenever `t >= expiry_ms`.
template <typename K, typename V>
class TTLLRUCache {
public:
    explicit TTLLRUCache(size_t capacity) : capacity_(capacity) {}

    size_t capacity() const noexcept { return capacity_; }

    // Inserts or updates `key` with `value` and TTL `ttl_ms` relative to `now_ms`.
    // - If capacity == 0, stores nothing.
    // - If ttl_ms <= 0, removes `key` if present and stores nothing.
    // - If `key` exists, updates value & expiry and promotes to MRU.
    // - If full when inserting a new key, evicts expired items (at `now_ms`) first;
    //   if still full, evicts the least-recently-used live item.
    void put(const K& key, V value, int64_t ttl_ms, int64_t now_ms = 0) {
        (void)key;
        (void)value;
        (void)ttl_ms;
        (void)now_ms;
        // TODO: implement
    }

    // Looks up `key` at `now_ms` in O(1) time.
    // - If missing or expired (`now_ms >= expiry_ms`), removes expired entry if present
    //   and returns std::nullopt.
    // - Otherwise promotes `key` to MRU and returns its value.
    std::optional<V> get(const K& key, int64_t now_ms) {
        (void)key;
        (void)now_ms;
        // TODO: implement
        return std::nullopt;
    }

    // Removes `key` if present in the cache. Returns true if removed, false otherwise.
    bool erase(const K& key) {
        (void)key;
        // TODO: implement
        return false;
    }

    // Removes all stored entries with `expiry_ms <= now_ms` and returns how many were removed.
    size_t purge_expired(int64_t now_ms) {
        (void)now_ms;
        // TODO: implement
        return 0;
    }

    // Returns the accurate count of non-expired entries at `now_ms`.
    size_t size(int64_t now_ms) {
        (void)now_ms;
        // TODO: implement
        return 0;
    }

private:
    size_t capacity_;
};
