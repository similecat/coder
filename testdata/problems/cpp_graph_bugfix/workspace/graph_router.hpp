#pragma once

#include <cstdint>
#include <optional>
#include <vector>

namespace routing {

struct Edge {
    int to;
    int64_t weight;
};

struct RouteResult {
    int64_t total_cost = 0;
    std::vector<int> path; // Ordered node IDs from src to dst inclusive
};

class GraphRouter {
public:
    explicit GraphRouter(int num_nodes);

    int num_nodes() const noexcept { return num_nodes_; }

    // Adds directed edge u -> v with non-negative weight.
    // Throws std::out_of_range if u or v is out of [0, num_nodes - 1].
    // Throws std::invalid_argument if weight < 0.
    void add_edge(int u, int v, int64_t weight);

    // Finds the optimal directed path from `src` to `dst`:
    //   1. Minimum total_cost (64-bit safe)
    //   2. Fewest hops (smallest path.size())
    //   3. Lexicographically smallest node vector `path` from `src` to `dst`
    // Returns std::nullopt if `dst` is unreachable from `src`.
    std::optional<RouteResult> shortest_path(int src, int dst) const;

private:
    int num_nodes_;
    std::vector<std::vector<Edge>> adj_;
};

} // namespace routing
