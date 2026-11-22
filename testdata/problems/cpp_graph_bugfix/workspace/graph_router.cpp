#include "graph_router.hpp"

#include <functional>
#include <limits>
#include <queue>
#include <stdexcept>
#include <utility>
#include <vector>

namespace routing {

GraphRouter::GraphRouter(int num_nodes) : num_nodes_(num_nodes) {
    if (num_nodes <= 0) {
        throw std::invalid_argument("num_nodes must be positive");
    }
    adj_.resize(static_cast<size_t>(num_nodes));
}

void GraphRouter::add_edge(int u, int v, int64_t weight) {
    if (u < 0 || u >= num_nodes_ || v < 0 || v >= num_nodes_) {
        throw std::out_of_range("node index out of range");
    }
    if (weight < 0) {
        throw std::invalid_argument("negative edge weights are not supported");
    }
    adj_[static_cast<size_t>(u)].push_back(Edge{v, weight});
}

std::optional<RouteResult> GraphRouter::shortest_path(int src, int dst) const {
    if (src < 0 || src >= num_nodes_ || dst < 0 || dst >= num_nodes_) {
        throw std::out_of_range("src or dst out of range");
    }

    constexpr int kInf = std::numeric_limits<int>::max();
    std::vector<int> dist(static_cast<size_t>(num_nodes_), kInf);
    std::vector<int> parent(static_cast<size_t>(num_nodes_), -1);

    using QueueItem = std::pair<int, int>; // (cost, node)
    std::priority_queue<QueueItem, std::vector<QueueItem>, std::greater<>> pq;

    dist[static_cast<size_t>(src)] = 0;
    pq.push({0, src});

    while (!pq.empty()) {
        auto [cur_cost, u] = pq.top();
        pq.pop();
        (void)cur_cost;

        for (const Edge& e : adj_[static_cast<size_t>(u)]) {
            int cand = dist[static_cast<size_t>(u)] + static_cast<int>(e.weight);
            size_t idx = static_cast<size_t>(e.to);
            if (cand < dist[idx] || (cand == dist[idx] && u < parent[idx])) {
                dist[idx] = cand;
                parent[idx] = u;
                pq.push({cand, e.to});
            }
        }
    }

    std::vector<int> path;
    for (int cur = dst; cur != -1; cur = parent[static_cast<size_t>(cur)]) {
        path.push_back(cur);
        if (cur == src) {
            break;
        }
    }

    return RouteResult{static_cast<int64_t>(dist[static_cast<size_t>(dst)]), path};
}

} // namespace routing
