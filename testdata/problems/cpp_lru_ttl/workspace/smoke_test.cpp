#include "lru_cache.hpp"

#include <cstdlib>
#include <iostream>
#include <string>

#define CHECK(cond)                                                                 \
    do {                                                                            \
        if (!(cond)) {                                                              \
            std::cerr << "SMOKE CHECK FAILED at line " << __LINE__ << ": " << #cond \
                      << std::endl;                                                 \
            return 1;                                                               \
        }                                                                           \
    } while (0)

int main() {
    TTLLRUCache<std::string, int> cache(2);

    cache.put("alpha", 10, 100, 1000); // expires at 1100
    cache.put("beta", 20, 100, 1010);  // expires at 1110
    CHECK(cache.size(1020) == 2);

    // Promote "alpha" to MRU so "beta" becomes LRU
    auto v1 = cache.get("alpha", 1020);
    CHECK(v1.has_value() && *v1 == 10);

    // Evicts LRU ("beta")
    cache.put("gamma", 30, 100, 1030);
    CHECK(!cache.get("beta", 1030).has_value());
    CHECK(cache.get("alpha", 1030).value_or(-1) == 10);
    CHECK(cache.get("gamma", 1030).value_or(-1) == 30);

    // Boundary expiration: alpha expires at 1100
    CHECK(!cache.get("alpha", 1100).has_value());
    CHECK(cache.size(1100) == 1);

    std::cout << "Smoke tests passed.\n";
    return 0;
}
