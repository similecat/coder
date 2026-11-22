"""Safe arithmetic and lazy variable formula evaluation engine.

IMPORTANT: Do not use Python's built-in eval(), exec(), or ast.literal_eval().
"""

from __future__ import annotations


class CycleError(ValueError):
    """Raised when evaluating variable formulas encounters a dependency cycle."""


class UnknownSymbolError(KeyError):
    """Raised when an expression references an undefined variable or function."""


class ExpressionEngine:
    """Evaluates arithmetic expressions and lazy variable formulas safely."""

    def __init__(self) -> None:
        # TODO: initialize symbol storage
        pass

    def set_value(self, name: str, value: int | float) -> None:
        """Assigns a constant numeric value (int or float, not bool) to `name`."""
        raise NotImplementedError("set_value is not implemented")

    def set_formula(self, name: str, formula: str) -> None:
        """Registers a lazy formula string for `name` after validating syntax."""
        raise NotImplementedError("set_formula is not implemented")

    def evaluate(self, expr: str) -> int | float:
        """Evaluates `expr` using current values and formulas."""
        raise NotImplementedError("evaluate is not implemented")
