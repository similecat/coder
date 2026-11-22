from __future__ import annotations

import heapq
from concurrent.futures import Future, ThreadPoolExecutor, wait, FIRST_COMPLETED
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
        attempts = 0
        max_attempts = max(1, spec.max_retries)
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
            except Exception as exc:
                last_err = exc

        return TaskResult(
            id=spec.id,
            status=TaskStatus.FAILED,
            error=last_err,
            attempts=attempts,
        )

    def run(self) -> dict[str, TaskResult]:
        if not self._tasks:
            return {}

        in_degree: dict[str, int] = {}
        dependents: dict[str, list[str]] = {tid: [] for tid in self._tasks}

        for tid, spec in self._tasks.items():
            unique_deps = list(dict.fromkeys(spec.deps))
            for dep in unique_deps:
                if dep not in self._tasks:
                    raise UnknownDependencyError(f"Task '{tid}' depends on unknown task '{dep}'")
                dependents[dep].append(tid)
            in_degree[tid] = len(unique_deps)

        ready_heap: list[tuple[int, str]] = []
        for tid, deg in in_degree.items():
            if deg == 0:
                spec = self._tasks[tid]
                heapq.heappush(ready_heap, (-spec.priority, tid))

        if not ready_heap:
            raise DAGCycleError("No root tasks found; graph contains a cycle")

        results: dict[str, TaskResult] = {}
        in_flight: dict[Future[TaskResult], str] = {}
        order_counter = 0

        with ThreadPoolExecutor(max_workers=self.max_workers) as pool:
            while ready_heap or in_flight:
                while ready_heap and len(in_flight) < self.max_workers:
                    _, tid = heapq.heappop(ready_heap)
                    spec = self._tasks[tid]
                    fut = pool.submit(self._run_with_retries, spec)
                    in_flight[fut] = tid

                if not in_flight:
                    break

                done, _ = wait(in_flight.keys(), return_when=FIRST_COMPLETED)
                for fut in done:
                    tid = in_flight.pop(fut)
                    res = fut.result()
                    res.execution_order = order_counter
                    order_counter += 1
                    results[tid] = res

                    if res.status == TaskStatus.FAILED:
                        for child_id in dependents[tid]:
                            if child_id not in results:
                                results[child_id] = TaskResult(
                                    id=child_id,
                                    status=TaskStatus.SKIPPED,
                                    attempts=0,
                                )
                            for grand_id in dependents[child_id]:
                                in_degree[grand_id] -= 1
                                if in_degree[grand_id] == 0 and grand_id not in results:
                                    gspec = self._tasks[grand_id]
                                    heapq.heappush(ready_heap, (-gspec.priority, grand_id))
                    else:
                        for child_id in dependents[tid]:
                            in_degree[child_id] -= 1
                            if in_degree[child_id] == 0 and child_id not in results:
                                cspec = self._tasks[child_id]
                                heapq.heappush(ready_heap, (-cspec.priority, child_id))

        return results
