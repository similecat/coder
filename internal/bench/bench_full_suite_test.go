package bench

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"coder/internal/types"
)

type fullFiveSolverProvider struct {
	mu    sync.Mutex
	steps map[string]int
}

func (f *fullFiveSolverProvider) Name() string         { return "mock-full-suite" }
func (f *fullFiveSolverProvider) DefaultModel() string { return "mock-full-v1" }
func (f *fullFiveSolverProvider) ListModels(context.Context) ([]types.ModelInfo, error) {
	return []types.ModelInfo{{ID: "mock-full-v1", Provider: "mock-full-suite"}}, nil
}
func (f *fullFiveSolverProvider) Close() error { return nil }

func (f *fullFiveSolverProvider) StreamChat(
	_ context.Context,
	req types.ChatRequest,
	onEvent func(types.StreamEvent),
) (*types.ChatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.steps == nil {
		f.steps = make(map[string]int)
	}

	first := req.Messages[0].Content
	var key, targetFile, content string
	var readTargets []string
	var loadSkill string

	switch {
	case strings.Contains(first, "TTLLRUCache"):
		key = "cpp_lru_ttl"
		readTargets = []string{"lru_cache.hpp"}
		targetFile = "lru_cache.hpp"
		content = referenceCPPLRUTTLHeader
	case strings.Contains(first, "GraphRouter") || strings.Contains(first, "graph_router"):
		key = "cpp_graph_bugfix"
		readTargets = []string{"graph_router.hpp", "graph_router.cpp"}
		targetFile = "graph_router.cpp"
		content = referenceCPPGraphRouterSource
	case strings.Contains(first, "ExpressionEngine"):
		key = "py_expr_engine"
		readTargets = []string{"expr_engine.py"}
		targetFile = "expr_engine.py"
		content = referencePyExprEngine
	case strings.Contains(first, "DAGScheduler"):
		key = "py_dag_scheduler_bugfix"
		readTargets = []string{"models.py", "scheduler.py"}
		targetFile = "scheduler.py"
		content = referencePyDAGScheduler
	case strings.Contains(first, "encode_packet"):
		key = "py_copilot_skill_compat"
		loadSkill = "telemetry-codec"
		readTargets = []string{"codec.py"}
		targetFile = "codec.py"
		content = referencePyTelemetryCodec
	default:
		return &types.ChatResponse{Content: "unrecognized benchmark prompt"}, nil
	}

	f.steps[key]++
	step := f.steps[key]

	switch step {
	case 1:
		var calls []types.ToolCall
		if loadSkill != "" {
			calls = append(calls, types.ToolCall{
				ID:        key + "-skill",
				Name:      "load_skill",
				Arguments: mustJSON(map[string]any{"skill_name": loadSkill}),
			})
		}
		for i, rt := range readTargets {
			calls = append(calls, types.ToolCall{
				ID:        key + "-read-" + string(rune('a'+i)),
				Name:      "read_file",
				Arguments: mustJSON(map[string]any{"path": rt}),
			})
		}
		return &types.ChatResponse{
			ToolCalls:    calls,
			InputTokens:  200,
			OutputTokens: 40,
		}, nil

	case 2:
		return &types.ChatResponse{
			ToolCalls: []types.ToolCall{
				{
					ID:   key + "-write",
					Name: "write_file",
					Arguments: mustJSON(map[string]any{
						"path":    targetFile,
						"content": content,
					}),
				},
			},
			InputTokens:  450,
			OutputTokens: 600,
		}, nil

	default:
		msg := "Completed and verified " + key + "."
		onEvent(types.StreamEvent{Type: types.EventTextDelta, Text: msg})
		return &types.ChatResponse{
			Content:      msg,
			InputTokens:  500,
			OutputTokens: 16,
		}, nil
	}
}

func TestEndToEndFullFiveProblemSuite(t *testing.T) {
	prov := &fullFiveSolverProvider{}
	report, err := RunSuite(context.Background(), SuiteOptions{
		ProblemsRoot:   filepath.Join("..", "..", "testdata", "problems"),
		UserHome:       t.TempDir(),
		CustomProvider: prov,
	})
	if err != nil {
		t.Fatalf("RunSuite failed: %v", err)
	}
	if report.TotalProblems != 5 || report.PassedProblems != 5 || report.FailedProblems != 0 {
		for _, r := range report.Results {
			t.Logf("[%s] %s (%s): err=%q\nverify:\n%s", r.Status, r.ProblemID, r.Language, r.Error, r.VerifyOutput)
		}
		t.Fatalf("expected 5/5 benchmark problems to PASS, got %d/%d", report.PassedProblems, report.TotalProblems)
	}
	t.Logf("\n%s", FormatScorecard(report))
}

const referenceCPPGraphRouterSource = `#include "graph_router.hpp"

#include <limits>
#include <queue>
#include <stdexcept>
#include <tuple>
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

    constexpr int64_t kInf = std::numeric_limits<int64_t>::max();
    const size_t n = static_cast<size_t>(num_nodes_);

    std::vector<int64_t> best_cost(n, kInf);
    std::vector<std::vector<int>> best_path(n);

    struct State {
        int64_t cost;
        std::vector<int> path;

        bool operator>(const State& other) const {
            if (cost != other.cost) {
                return cost > other.cost;
            }
            if (path.size() != other.path.size()) {
                return path.size() > other.path.size();
            }
            return path > other.path;
        }
    };

    std::priority_queue<State, std::vector<State>, std::greater<State>> pq;
    best_cost[static_cast<size_t>(src)] = 0;
    best_path[static_cast<size_t>(src)] = {src};
    pq.push(State{0, {src}});

    while (!pq.empty()) {
        State cur = std::move(pq.top());
        pq.pop();

        const int u = cur.path.back();
        const size_t u_idx = static_cast<size_t>(u);

        if (cur.cost != best_cost[u_idx] || cur.path != best_path[u_idx]) {
            continue;
        }
        if (u == dst) {
            return RouteResult{cur.cost, std::move(cur.path)};
        }

        for (const Edge& e : adj_[u_idx]) {
            if (cur.cost > kInf - e.weight) {
                continue;
            }
            const int64_t next_cost = cur.cost + e.weight;
            const size_t v_idx = static_cast<size_t>(e.to);

            if (next_cost > best_cost[v_idx]) {
                continue;
            }

            std::vector<int> cand_path = cur.path;
            cand_path.push_back(e.to);

            bool better = false;
            if (best_path[v_idx].empty() || next_cost < best_cost[v_idx]) {
                better = true;
            } else if (cand_path.size() != best_path[v_idx].size()) {
                better = cand_path.size() < best_path[v_idx].size();
            } else {
                better = cand_path < best_path[v_idx];
            }

            if (better) {
                best_cost[v_idx] = next_cost;
                best_path[v_idx] = cand_path;
                pq.push(State{next_cost, std::move(cand_path)});
            }
        }
    }

    const size_t dst_idx = static_cast<size_t>(dst);
    if (best_cost[dst_idx] == kInf) {
        return std::nullopt;
    }
    return RouteResult{best_cost[dst_idx], best_path[dst_idx]};
}

} // namespace routing
`

const referencePyExprEngine = `"""Safe AST-visitor formula and expression engine with cycle detection."""

from __future__ import annotations

import ast
import math
from typing import Union

Number = Union[int, float]


class CycleError(ValueError):
    """Raised when variable formulas contain a dependency cycle."""


class UnknownSymbolError(KeyError):
    """Raised when referencing an undefined variable or function."""


_ALLOWED_AST_NODES = (
    ast.Expression,
    ast.BinOp,
    ast.UnaryOp,
    ast.Call,
    ast.Name,
    ast.Load,
    ast.Constant,
    ast.Add,
    ast.Sub,
    ast.Mult,
    ast.Div,
    ast.FloorDiv,
    ast.Mod,
    ast.Pow,
    ast.UAdd,
    ast.USub,
)


def _validate_ast(tree: ast.AST) -> ast.Expression:
    if not isinstance(tree, ast.Expression):
        raise ValueError("Expected single expression")
    for node in ast.walk(tree):
        if not isinstance(node, _ALLOWED_AST_NODES):
            raise ValueError(f"Unsupported syntax node: {type(node).__name__}")
        if isinstance(node, ast.Constant):
            if isinstance(node.value, bool) or not isinstance(node.value, (int, float)):
                raise ValueError("Only int and float numeric constants are permitted")
        if isinstance(node, ast.Call):
            if not isinstance(node.func, ast.Name):
                raise ValueError("Only direct named function calls are allowed")
            if node.keywords:
                raise ValueError("Keyword arguments are not supported")
    return tree


class ExpressionEngine:
    """Evaluates arithmetic expressions and lazy variable formulas safely."""

    def __init__(self) -> None:
        self._values: dict[str, Number] = {}
        self._formulas: dict[str, ast.Expression] = {}

    def set_value(self, name: str, value: Number) -> None:
        if not isinstance(name, str) or not name.isidentifier():
            raise ValueError(f"Invalid variable identifier: {name!r}")
        if isinstance(value, bool) or not isinstance(value, (int, float)):
            raise TypeError("Value must be an int or float (not bool)")
        self._formulas.pop(name, None)
        self._values[name] = value

    def set_formula(self, name: str, formula: str) -> None:
        if not isinstance(name, str) or not name.isidentifier():
            raise ValueError(f"Invalid variable identifier: {name!r}")
        if not isinstance(formula, str) or not formula.strip():
            raise ValueError("Formula must be a non-empty string")
        parsed = _validate_ast(ast.parse(formula, mode="eval"))
        self._values.pop(name, None)
        self._formulas[name] = parsed

    def evaluate(self, expr: str) -> Number:
        if not isinstance(expr, str) or not expr.strip():
            raise ValueError("Expression must be a non-empty string")
        parsed = _validate_ast(ast.parse(expr, mode="eval"))
        memo: dict[str, Number] = {}
        visiting: set[str] = set()
        return self._eval_node(parsed.body, visiting, memo)

    def _resolve_symbol(self, name: str, visiting: set[str], memo: dict[str, Number]) -> Number:
        if name in memo:
            return memo[name]
        if name in visiting:
            raise CycleError(f"Cyclic formula dependency involving '{name}'")
        if name in self._values:
            return self._values[name]
        if name in self._formulas:
            visiting.add(name)
            try:
                val = self._eval_node(self._formulas[name].body, visiting, memo)
            finally:
                visiting.remove(name)
            memo[name] = val
            return val
        raise UnknownSymbolError(name)

    def _eval_node(self, node: ast.AST, visiting: set[str], memo: dict[str, Number]) -> Number:
        if isinstance(node, ast.Constant):
            val = node.value
            if isinstance(val, bool) or not isinstance(val, (int, float)):
                raise ValueError("Invalid constant")
            return val

        if isinstance(node, ast.Name):
            return self._resolve_symbol(node.id, visiting, memo)

        if isinstance(node, ast.UnaryOp):
            operand = self._eval_node(node.operand, visiting, memo)
            if isinstance(node.op, ast.UAdd):
                return +operand
            if isinstance(node.op, ast.USub):
                return -operand
            raise ValueError("Unsupported unary operator")

        if isinstance(node, ast.BinOp):
            left = self._eval_node(node.left, visiting, memo)
            right = self._eval_node(node.right, visiting, memo)
            op = node.op
            if isinstance(op, ast.Add):
                return left + right
            if isinstance(op, ast.Sub):
                return left - right
            if isinstance(op, ast.Mult):
                return left * right
            if isinstance(op, ast.Div):
                return left / right
            if isinstance(op, ast.FloorDiv):
                return left // right
            if isinstance(op, ast.Mod):
                return left % right
            if isinstance(op, ast.Pow):
                res = left ** right
                if isinstance(res, complex):
                    raise ValueError("Complex results are not supported")
                return res
            raise ValueError("Unsupported binary operator")

        if isinstance(node, ast.Call) and isinstance(node.func, ast.Name):
            fn_name = node.func.id
            args = [self._eval_node(arg, visiting, memo) for arg in node.args]
            if fn_name == "abs":
                if len(args) != 1:
                    raise TypeError("abs() takes exactly 1 argument")
                return abs(args[0])
            if fn_name == "round":
                if len(args) == 1:
                    return round(args[0])
                if len(args) == 2:
                    if isinstance(args[1], float) or isinstance(args[1], bool):
                        if not isinstance(args[1], int):
                            raise TypeError("round() ndigits must be int")
                    return round(args[0], int(args[1]))
                raise TypeError("round() takes 1 or 2 arguments")
            if fn_name == "min":
                if len(args) < 2:
                    raise TypeError("min() requires at least 2 arguments")
                return min(args)
            if fn_name == "max":
                if len(args) < 2:
                    raise TypeError("max() requires at least 2 arguments")
                return max(args)
            raise UnknownSymbolError(fn_name)

        raise ValueError(f"Unsupported AST node: {type(node).__name__}")


__all__ = ["ExpressionEngine", "CycleError", "UnknownSymbolError"]
`

const referencePyDAGScheduler = `from __future__ import annotations

from collections import deque
import heapq
from concurrent.futures import FIRST_COMPLETED, Future, ThreadPoolExecutor, wait
from typing import Iterable

from models import (
    DAGCycleError,
    NonRetryableError,
    TaskResult,
    TaskSpec,
    TaskStatus,
    UnknownDependencyError,
)


class DAGScheduler:
    """Topological and priority-aware concurrent DAG task scheduler."""

    def __init__(self, max_workers: int = 1) -> None:
        if max_workers < 1:
            raise ValueError("max_workers must be >= 1")
        self.max_workers = max_workers
        self._tasks: dict[str, TaskSpec] = {}

    def add_task(self, spec: TaskSpec) -> None:
        if spec.max_retries < 0:
            raise ValueError("max_retries must be >= 0")
        if spec.id in self._tasks:
            raise ValueError(f"Duplicate task id: {spec.id}")
        self._tasks[spec.id] = spec

    def add_tasks(self, specs: Iterable[TaskSpec]) -> None:
        for spec in specs:
            self.add_task(spec)

    def _run_with_retries(self, spec: TaskSpec) -> TaskResult:
        if spec.max_retries < 0:
            raise ValueError("max_retries must be >= 0")
        attempts = 0
        max_attempts = 1 + spec.max_retries
        last_err: BaseException | None = None

        while attempts < max_attempts:
            attempts += 1
            try:
                val = spec.fn()
                return TaskResult(
                    id=spec.id,
                    status=TaskStatus.SUCCEEDED,
                    value=val,
                    attempts=attempts,
                )
            except BaseException as exc:
                last_err = exc
                if isinstance(exc, NonRetryableError):
                    break
                if not spec.retryable_exceptions or not isinstance(exc, spec.retryable_exceptions):
                    break

        return TaskResult(
            id=spec.id,
            status=TaskStatus.FAILED,
            error=last_err,
            attempts=attempts,
        )

    def run(self) -> dict[str, TaskResult]:
        if not self._tasks:
            return {}

        for spec in self._tasks.values():
            if spec.max_retries < 0:
                raise ValueError("max_retries must be >= 0")

        in_degree: dict[str, int] = {}
        dependents: dict[str, list[str]] = {tid: [] for tid in self._tasks}

        for tid, spec in self._tasks.items():
            unique_deps = list(dict.fromkeys(spec.deps))
            for dep in unique_deps:
                if dep not in self._tasks:
                    raise UnknownDependencyError(f"Task '{tid}' depends on unknown task '{dep}'")
                dependents[dep].append(tid)
            in_degree[tid] = len(unique_deps)

        # Upfront Kahn topological check so ANY cycle (including disconnected or downstream cycles) raises DAGCycleError.
        kahn_deg = dict(in_degree)
        q = deque(tid for tid, deg in kahn_deg.items() if deg == 0)
        visited = 0
        while q:
            u = q.popleft()
            visited += 1
            for v in dependents[u]:
                kahn_deg[v] -= 1
                if kahn_deg[v] == 0:
                    q.append(v)
        if visited != len(self._tasks):
            raise DAGCycleError("Task dependency graph contains a cycle")

        ready_heap: list[tuple[int, str]] = []
        for tid, deg in in_degree.items():
            if deg == 0:
                spec = self._tasks[tid]
                heapq.heappush(ready_heap, (-spec.priority, tid))

        results: dict[str, TaskResult] = {}
        in_flight: dict[Future[TaskResult], str] = {}
        order_counter = 0

        def propagate_skip(root_failed_id: str) -> None:
            bfs = deque(dependents[root_failed_id])
            while bfs:
                cur_id = bfs.popleft()
                if cur_id in results:
                    continue
                results[cur_id] = TaskResult(
                    id=cur_id,
                    status=TaskStatus.SKIPPED,
                    attempts=0,
                )
                for nxt in dependents[cur_id]:
                    if nxt not in results:
                        bfs.append(nxt)

        with ThreadPoolExecutor(max_workers=self.max_workers) as pool:
            while ready_heap or in_flight:
                while ready_heap and len(in_flight) < self.max_workers:
                    _, tid = heapq.heappop(ready_heap)
                    if tid in results:
                        continue
                    spec = self._tasks[tid]
                    fut = pool.submit(self._run_with_retries, spec)
                    in_flight[fut] = tid

                if not in_flight:
                    break

                done, _ = wait(in_flight.keys(), return_when=FIRST_COMPLETED)
                done_ids = sorted(done, key=lambda f: in_flight[f])
                for fut in done_ids:
                    tid = in_flight.pop(fut)
                    res = fut.result()
                    res.execution_order = order_counter
                    order_counter += 1
                    results[tid] = res

                    if res.status == TaskStatus.FAILED:
                        propagate_skip(tid)
                    else:
                        for child_id in dependents[tid]:
                            if child_id in results:
                                continue
                            in_degree[child_id] -= 1
                            if in_degree[child_id] == 0:
                                cspec = self._tasks[child_id]
                                heapq.heappush(ready_heap, (-cspec.priority, child_id))

        return results
`
