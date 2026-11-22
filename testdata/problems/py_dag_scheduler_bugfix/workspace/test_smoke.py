import unittest

from models import TaskSpec, TaskStatus
from scheduler import DAGScheduler


class SmokeSchedulerTests(unittest.TestCase):
    def test_simple_diamond(self) -> None:
        log: list[str] = []
        sched = DAGScheduler(max_workers=1)
        sched.add_tasks(
            [
                TaskSpec("a", fn=lambda: log.append("a") or 1),
                TaskSpec("b", fn=lambda: log.append("b") or 2, deps=["a"], priority=5),
                TaskSpec("c", fn=lambda: log.append("c") or 3, deps=["a"], priority=10),
                TaskSpec("d", fn=lambda: log.append("d") or 4, deps=["b", "c"]),
            ]
        )
        res = sched.run()
        self.assertEqual(res["d"].status, TaskStatus.SUCCEEDED)
        self.assertEqual(log, ["a", "c", "b", "d"])


if __name__ == "__main__":
    unittest.main()
