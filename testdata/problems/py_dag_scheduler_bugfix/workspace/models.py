from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from typing import Any, Callable


class TaskStatus(str, Enum):
    PENDING = "PENDING"
    RUNNING = "RUNNING"
    SUCCEEDED = "SUCCEEDED"
    FAILED = "FAILED"
    SKIPPED = "SKIPPED"


class DAGError(Exception):
    """Base exception for DAG scheduler errors."""


class DAGCycleError(DAGError):
    """Raised when the task dependency graph contains a cycle."""


class UnknownDependencyError(DAGError, KeyError):
    """Raised when a task depends on an unknown task ID."""


class NonRetryableError(Exception):
    """Exception indicating that a task failure must not be retried."""


@dataclass
class TaskSpec:
    id: str
    fn: Callable[[], Any]
    deps: list[str] = field(default_factory=list)
    priority: int = 0
    max_retries: int = 0
    retryable_exceptions: tuple[type[BaseException], ...] = (Exception,)


@dataclass
class TaskResult:
    id: str
    status: TaskStatus
    value: Any = None
    error: BaseException | None = None
    attempts: int = 0
    execution_order: int | None = None
