#include "graph_router.hpp"

#include <iostream>

int main() {
    routing::GraphRouter router(4);
    router.add_edge(0, 1, 10);
    router.add_edge(1, 3, 15);
    router.add_edge(0, 2, 5);
    router.add_edge(2, 3, 12);

    auto res = router.shortest_path(0, 3);
    if (!res.has_value() || res->total_cost != 17) {
        std::cerr << "Unexpected total cost\n";
        return 1;
    }

    std::cout << "Computed cost: " << res->total_cost << " (nodes=" << res->path.size() << ")\n";
    return 0;
}
