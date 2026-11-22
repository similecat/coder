#include "graph_router.hpp"

#include <cstdint>
#include <cstdlib>
#include <iostream>
#include <stdexcept>
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

void test_large_weights_64bit() {
    routing::GraphRouter g(7);
    constexpr int64_t kBillion = 1'000'000'000LL;

    // 5-edge chain: total cost = 5 * 10^9 (> INT32_MAX)
    for (int i = 0; i < 5; ++i) {
        g.add_edge(i, i + 1, kBillion);
    }
    // Direct shortcut with slightly higher cost = 6 * 10^9
    g.add_edge(0, 5, 6 * kBillion);
    // Node 6 is isolated / unreachable

    auto res = g.shortest_path(0, 5);
    VERIFY(res.has_value());
    VERIFY(res->total_cost == 5 * kBillion);
    VERIFY((res->path == std::vector<int>{0, 1, 2, 3, 4, 5}));

    // Unreachable node must return nullopt without overflow
    VERIFY(!g.shortest_path(0, 6).has_value());
}

void test_equal_cost_tie_breaking() {
    // 1. Fewer hops wins before lexicographical node IDs:
    //    0 -> 4 (cost 10, 2 nodes: {0, 4})
    //    0 -> 1 -> 2 -> 4 (cost 10, 4 nodes: {0, 1, 2, 4})
    {
        routing::GraphRouter g(5);
        g.add_edge(0, 1, 3);
        g.add_edge(1, 2, 3);
        g.add_edge(2, 4, 4);
        g.add_edge(0, 4, 10);

        auto res = g.shortest_path(0, 4);
        VERIFY(res.has_value());
        VERIFY(res->total_cost == 10);
        VERIFY((res->path == std::vector<int>{0, 4}));
    }

    // 2. Equal cost AND equal hop count -> lexicographically smallest path from src:
    //    Path A: 0 -> 1 -> 4 -> 5 (cost 2 + 3 + 5 = 10, 4 nodes)
    //    Path B: 0 -> 2 -> 3 -> 5 (cost 4 + 1 + 5 = 10, 4 nodes)
    //    Notice at node 5, predecessor in Path B is 3 < 4, YET from src=0
    //    {0, 1, 4, 5} < {0, 2, 3, 5} at index 1!
    {
        routing::GraphRouter g(6);
        g.add_edge(0, 2, 4);
        g.add_edge(2, 3, 1);
        g.add_edge(3, 5, 5);

        g.add_edge(0, 1, 2);
        g.add_edge(1, 4, 3);
        g.add_edge(4, 5, 5);

        auto res = g.shortest_path(0, 5);
        VERIFY(res.has_value());
        VERIFY(res->total_cost == 10);
        VERIFY((res->path == std::vector<int>{0, 1, 4, 5}));
    }

    // 3. Stale priority-queue entries must not corrupt relaxation or path choice
    {
        routing::GraphRouter g(5);
        g.add_edge(0, 3, 100);
        g.add_edge(0, 2, 1);
        g.add_edge(2, 1, 1);
        g.add_edge(1, 3, 1);
        g.add_edge(3, 4, 5);

        auto res = g.shortest_path(0, 4);
        VERIFY(res.has_value());
        VERIFY(res->total_cost == 8);
        VERIFY((res->path == std::vector<int>{0, 2, 1, 3, 4}));
    }
}

void test_unreachable_self_loops_and_cycles() {
    routing::GraphRouter g(5);
    // Component 1 with zero-weight cycle and self-loops: 0 <-> 1, 0 -> 0, 1 -> 2
    g.add_edge(0, 0, 0);
    g.add_edge(0, 1, 0);
    g.add_edge(1, 0, 0);
    g.add_edge(1, 1, 3);
    g.add_edge(1, 2, 7);
    g.add_edge(2, 0, 2);

    // Component 2: 3 <-> 4 (unreachable from 0..2)
    g.add_edge(3, 4, 1);
    g.add_edge(4, 3, 1);

    // src == dst with 0-weight self-loop and 0-weight cycle
    auto self_res = g.shortest_path(0, 0);
    VERIFY(self_res.has_value());
    VERIFY(self_res->total_cost == 0);
    VERIFY((self_res->path == std::vector<int>{0}));

    // Shortest path across 0-weight cycle without infinite loop
    auto cycle_res = g.shortest_path(0, 2);
    VERIFY(cycle_res.has_value());
    VERIFY(cycle_res->total_cost == 7);
    VERIFY((cycle_res->path == std::vector<int>{0, 1, 2}));

    // Unreachable component
    VERIFY(!g.shortest_path(0, 4).has_value());
    VERIFY(!g.shortest_path(2, 3).has_value());
}

void test_validation_exceptions() {
    bool threw_bad_nodes = false;
    try {
        routing::GraphRouter bad(0);
    } catch (const std::invalid_argument&) {
        threw_bad_nodes = true;
    }
    VERIFY(threw_bad_nodes);

    routing::GraphRouter g(3);

    bool threw_neg = false;
    try {
        g.add_edge(0, 1, -1);
    } catch (const std::invalid_argument&) {
        threw_neg = true;
    }
    VERIFY(threw_neg);

    bool threw_oor_edge = false;
    try {
        g.add_edge(0, 3, 5);
    } catch (const std::out_of_range&) {
        threw_oor_edge = true;
    }
    VERIFY(threw_oor_edge);

    bool threw_oor_query = false;
    try {
        (void)g.shortest_path(-1, 2);
    } catch (const std::out_of_range&) {
        threw_oor_query = true;
    }
    VERIFY(threw_oor_query);
}

} // namespace

int main() {
    test_large_weights_64bit();
    test_equal_cost_tie_breaking();
    test_unreachable_self_loops_and_cycles();
    test_validation_exceptions();

    std::cout << "All cpp_graph_bugfix verification checks passed.\n";
    return 0;
}
