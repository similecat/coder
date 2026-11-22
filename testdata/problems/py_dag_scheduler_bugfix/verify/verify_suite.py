from __future__ import annotations

import os
import sys
import threading
import time
import unittest
from concurrent.futures import ThreadPoolExecutor, TimeoutError as FuturesTimeoutError
from pathlib import Path


def _resolve_workspace(marker_file: str) -> Path:
    env_dir = os.environ.get("WORKSPACE_DIR")
    if env_dir:
        p = Path(env_dir).resolve()
        if (p / marker_file).exists():
            return p
    cwd = Path.cwd().resolve()
    if (cwd / marker_file).exists():
        return cwd
    here = Path(__file__).resolve().parent
    if (here / marker_file).exists():
        return here
    if (here.parent / marker_file).exists():
        return here.parent
    if (here.parent / "workspace" / marker_file).exists():
        return (here.parent / "workspace").resolve()
    return cwd


WORKSPACE_DIR = _resolve_workspace("scheduler.py")
if str(WORKSPACE_DIR) not in sys.path:
    sys.path.insert(0, str(WORKSPACE_DIR))

from models import (  # noqa: E402
    DAGCycleError,
    NonRetryableError,
    TaskResult,
    TaskSpec,
    TaskStatus,
)
from scheduler import DAGScheduler  # noqa: E402


def _run_with_timeout(sched: DAGScheduler, timeout_sec: float = 5.0) -> dict[str, TaskResult]:
    with ThreadPoolExecutor(max_workers=1) as guard:
        fut = guard.submit(sched.run)
        try:
            return fut.result(timeout=timeout_sec)
        except FuturesTimeoutError as exc:
            raise AssertionError(f"scheduler.run() hung beyond {timeout_sec}s") from exc


class VerifyDAGSchedulerSuite(unittest.TestCase):
    def test_four_level_transitive_skip_and_join(self) -> None:
        executed: list[str] = []

        def fail_root() -> None:
            executed.append("L1")
            raise RuntimeError("boom at L1")

        def make_ok(name: str):
            def _fn() -> str:
                executed.append(name)
                return name

            return _fn

        sched = DAGScheduler(max_workers=2)
        sched.add_tasks(
            [
                # 4-level chain: L1 -> L2 -> L3 -> L4
                TaskSpec("L1", fn=fail_root),
                TaskSpec("L2", fn=make_ok("L2"), deps=["L1"]),
                TaskSpec("L3", fn=make_ok("L3"), deps=["L2"]),
                TaskSpec("L4", fn=make_ok("L4"), deps=["L3"]),
                # Independent healthy branch: I1 -> I2
                TaskSpec("I1", fn=make_ok("I1")),
                TaskSpec("I2", fn=make_ok("I2"), deps=["I1"]),
                # Multi-parent merge depending on healthy I2 and skipped L3
                TaskSpec("MERGE", fn=make_ok("MERGE"), deps=["I2", "L3"]),
            ]
        )

        res = _run_with_timeout(sched)
        self.assertEqual(set(res.keys()), {"L1", "L2", "L3", "L4", "I1", "I2", "MERGE"})
        self.assertEqual(res["L1"].status, TaskStatus.FAILED)
        self.assertEqual(res["L1"].attempts, 1)

        for skipped_id in ("L2", "L3", "L4", "MERGE"):
            self.assertEqual(res[skipped_id].status, TaskStatus.SKIPPED, f"{skipped_id} should be SKIPPED")
            self.assertEqual(res[skipped_id].attempts, 0)
            self.assertNotIn(skipped_id, executed)

        self.assertEqual(res["I1"].status, TaskStatus.SUCCEEDED)
        self.assertEqual(res["I2"].status, TaskStatus.SUCCEEDED)

    def test_exact_retry_counts_and_non_retryable_exceptions(self) -> None:
        # Case 1: max_retries=2 -> fails twice, succeeds on 3rd attempt (total 3 calls)
        flaky_calls = 0

        def flaky_succeeds_on_third() -> str:
            nonlocal flaky_calls
            flaky_calls += 1
            if flaky_calls < 3:
                raise ConnectionError(f"transient #{flaky_calls}")
            return "recovered"

        # Case 2: max_retries=2 -> fails all 3 attempts
        always_fail_calls = 0

        def always_fails() -> None:
            nonlocal always_fail_calls
            always_fail_calls += 1
            raise TimeoutError("timed out")

        # Case 3: max_retries=4 but raises NonRetryableError -> must stop after 1 attempt
        non_retry_calls = 0

        def raises_non_retryable() -> None:
            nonlocal non_retry_calls
            non_retry_calls += 1
            raise NonRetryableError("fatal config")

        # Case 4: max_retries=3 with retryable_exceptions=(TimeoutError,), raises ValueError -> 1 attempt
        unmatched_exc_calls = 0

        def raises_unmatched() -> None:
            nonlocal unmatched_exc_calls
            unmatched_exc_calls += 1
            raise ValueError("bad input")

        sched = DAGScheduler(max_workers=2)
        sched.add_tasks(
            [
                TaskSpec("flaky", fn=flaky_succeeds_on_third, max_retries=2),
                TaskSpec("after_flaky", fn=lambda: "ok", deps=["flaky"]),
                TaskSpec("exhaust", fn=always_fails, max_retries=2),
                TaskSpec("after_exhaust", fn=lambda: "never", deps=["exhaust"]),
                TaskSpec("fatal", fn=raises_non_retryable, max_retries=4),
                TaskSpec(
                    "selective",
                    fn=raises_unmatched,
                    max_retries=3,
                    retryable_exceptions=(TimeoutError,),
                ),
            ]
        )

        res = _run_with_timeout(sched)

        self.assertEqual(flaky_calls, 3)
        self.assertEqual(res["flaky"].status, TaskStatus.SUCCEEDED)
        self.assertEqual(res["flaky"].value, "recovered")
        self.assertEqual(res["flaky"].attempts, 3)
        self.assertEqual(res["after_flaky"].status, TaskStatus.SUCCEEDED)

        self.assertEqual(always_fail_calls, 3)
        self.assertEqual(res["exhaust"].status, TaskStatus.FAILED)
        self.assertEqual(res["exhaust"].attempts, 3)
        self.assertEqual(res["after_exhaust"].status, TaskStatus.SKIPPED)

        self.assertEqual(non_retry_calls, 1)
        self.assertEqual(res["fatal"].status, TaskStatus.FAILED)
        self.assertEqual(res["fatal"].attempts, 1)

        self.assertEqual(unmatched_exc_calls, 1)
        self.assertEqual(res["selective"].status, TaskStatus.FAILED)
        self.assertEqual(res["selective"].attempts, 1)

    def test_diamond_topological_and_parallel_execution(self) -> None:
        lock = threading.Lock()
        events: list[tuple[str, str]] = []

        def make_timed(name: str, delay: float = 0.03):
            def _fn() -> str:
                with lock:
                    events.append(("start", name))
                time.sleep(delay)
                with lock:
                    events.append(("end", name))
                return name

            return _fn

        sched = DAGScheduler(max_workers=4)
        sched.add_tasks(
            [
                TaskSpec("root", fn=make_timed("root", 0.01)),
                TaskSpec("left", fn=make_timed("left", 0.04), deps=["root"]),
                TaskSpec("right", fn=make_timed("right", 0.04), deps=["root"]),
                TaskSpec("sink", fn=make_timed("sink", 0.01), deps=["left", "right"]),
            ]
        )

        res = _run_with_timeout(sched)
        for tid in ("root", "left", "right", "sink"):
            self.assertEqual(res[tid].status, TaskStatus.SUCCEEDED)

        self.assertLess(events.index(("end", "root")), events.index(("start", "left")))
        self.assertLess(events.index(("end", "root")), events.index(("start", "right")))
        self.assertLess(events.index(("end", "left")), events.index(("start", "sink")))
        self.assertLess(events.index(("end", "right")), events.index(("start", "sink")))

    def test_priority_ordering_and_tie_breaking(self) -> None:
        order: list[str] = []

        def rec(name: str):
            return lambda: order.append(name) or name

        sched = DAGScheduler(max_workers=1)
        sched.add_tasks(
            [
                TaskSpec("gate", fn=rec("gate"), priority=0),
                TaskSpec("low", fn=rec("low"), deps=["gate"], priority=1),
                TaskSpec("high_b", fn=rec("high_b"), deps=["gate"], priority=20),
                TaskSpec("mid", fn=rec("mid"), deps=["gate"], priority=10),
                TaskSpec("high_a", fn=rec("high_a"), deps=["gate"], priority=20),
            ]
        )

        res = _run_with_timeout(sched)
        self.assertEqual(len(res), 5)
        self.assertEqual(order, ["gate", "high_a", "high_b", "mid", "low"])

    def test_cycle_detection_including_downstream_cycles(self) -> None:
        # 1. Pure 3-node cycle
        s1 = DAGScheduler(max_workers=2)
        s1.add_tasks(
            [
                TaskSpec("a", fn=lambda: 1, deps=["c"]),
                TaskSpec("b", fn=lambda: 2, deps=["a"]),
                TaskSpec("c", fn=lambda: 3, deps=["b"]),
            ]
        )
        with self.assertRaises(DAGCycleError):
            _run_with_timeout(s1)

        # 2. Valid root task + downstream cycle (must NOT silently return partial results!)
        s2 = DAGScheduler(max_workers=2)
        s2.add_tasks(
            [
                TaskSpec("root", fn=lambda: "ok"),
                TaskSpec("x", fn=lambda: 1, deps=["root", "y"]),
                TaskSpec("y", fn=lambda: 2, deps=["x"]),
            ]
        )
        with self.assertRaises(DAGCycleError):
            _run_with_timeout(s2)

        # 3. Self-dependency alongside valid task
        s3 = DAGScheduler(max_workers=1)
        s3.add_tasks(
            [
                TaskSpec("ok", fn=lambda: 1),
                TaskSpec("self_loop", fn=lambda: 2, deps=["self_loop"]),
            ]
        )
        with self.assertRaises(DAGCycleError):
            _run_with_timeout(s3)

        # 4. Unknown dependency
        s4 = DAGScheduler(max_workers=1)
        s4.add_task(TaskSpec("orphan", fn=lambda: 1, deps=["does_not_exist"]))
        with self.assertRaises(KeyError):
            _run_with_timeout(s4)


if __name__ == "__main__":
    unittest.main()
