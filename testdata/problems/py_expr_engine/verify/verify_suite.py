from __future__ import annotations

import ast
import os
import sys
import unittest
from pathlib import Path
from unittest import mock


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


WORKSPACE_DIR = _resolve_workspace("expr_engine.py")
if str(WORKSPACE_DIR) not in sys.path:
    sys.path.insert(0, str(WORKSPACE_DIR))

from expr_engine import CycleError, ExpressionEngine, UnknownSymbolError  # noqa: E402


class VerifyExpressionEngineSuite(unittest.TestCase):
    def test_ban_eval_and_exec_in_workspace_sources(self) -> None:
        forbidden_names = {"eval", "exec", "literal_eval"}
        checked_files = 0

        for py_file in WORKSPACE_DIR.glob("*.py"):
            if py_file.name.startswith(("test_", "verify_")):
                continue
            checked_files += 1
            tree = ast.parse(py_file.read_text(encoding="utf-8-sig"), filename=str(py_file))
            for node in ast.walk(tree):
                if isinstance(node, ast.Call):
                    func = node.func
                    if isinstance(func, ast.Name) and func.id in forbidden_names:
                        self.fail(f"Forbidden call '{func.id}()' in {py_file.name}")
                    if isinstance(func, ast.Attribute) and func.attr in forbidden_names:
                        self.fail(f"Forbidden attribute call '.{func.attr}()' in {py_file.name}")
                    if (
                        isinstance(func, ast.Name)
                        and func.id == "getattr"
                        and len(node.args) >= 2
                        and isinstance(node.args[1], ast.Constant)
                        and node.args[1].value in forbidden_names
                    ):
                        self.fail(f"Forbidden getattr for '{node.args[1].value}' in {py_file.name}")

        self.assertGreaterEqual(checked_files, 1)

        with (
            mock.patch("builtins.eval", side_effect=AssertionError("builtins.eval is banned")),
            mock.patch("builtins.exec", side_effect=AssertionError("builtins.exec is banned")),
            mock.patch("ast.literal_eval", side_effect=AssertionError("ast.literal_eval is banned")),
        ):
            eng = ExpressionEngine()
            eng.set_value("x", 3)
            eng.set_formula("y", "x ** 2 + 1")
            self.assertEqual(eng.evaluate("y * 2"), 20)

    def test_precedence_right_associative_power_and_unary(self) -> None:
        eng = ExpressionEngine()
        # Right-associative exponentiation: 2 ** (3 ** 2) == 2 ** 9 == 512
        self.assertEqual(eng.evaluate("2 ** 3 ** 2"), 512)
        self.assertIsInstance(eng.evaluate("2 ** 3 ** 2"), int)

        # Unary minus binds looser than ** on the left, allowed on the right
        self.assertEqual(eng.evaluate("-2 ** 2"), -4)
        self.assertEqual(eng.evaluate("(-2) ** 2"), 4)
        self.assertAlmostEqual(eng.evaluate("2 ** -3"), 0.125)
        self.assertEqual(eng.evaluate("-+-5"), 5)
        self.assertEqual(eng.evaluate("--+-5"), -5)

        # Floor division, modulo, true division, parentheses
        self.assertEqual(eng.evaluate("17 // 5"), 3)
        self.assertEqual(eng.evaluate("17 % 5"), 2)
        self.assertAlmostEqual(eng.evaluate("7 / 2"), 3.5)
        self.assertAlmostEqual(eng.evaluate("(1.5 + 2.5) * 3 - 4 / 8"), 11.5)

    def test_builtin_functions_nested(self) -> None:
        eng = ExpressionEngine()
        self.assertEqual(eng.evaluate("min(10, abs(-3 * 4), 7)"), 7)
        self.assertEqual(eng.evaluate("max(1, 9 - 2, abs(-15), 4)"), 15)
        self.assertEqual(eng.evaluate("round(3.6)"), 4)
        self.assertAlmostEqual(eng.evaluate("round(3.14159, 3)"), 3.142)

    def test_diamond_dependencies_and_dynamic_updates(self) -> None:
        eng = ExpressionEngine()
        # Forward reference before 'a' exists
        eng.set_formula("d", "b + c")
        eng.set_formula("b", "a + 1")
        eng.set_formula("c", "a * 2")
        eng.set_value("a", 5)

        # b = 6, c = 10 -> d = 16 (shared node 'a' must NOT trigger CycleError)
        self.assertEqual(eng.evaluate("d"), 16)
        self.assertEqual(eng.evaluate("d + a"), 21)

        # Dynamic update of 'a' propagates lazily
        eng.set_value("a", 12)
        self.assertEqual(eng.evaluate("d"), 37)  # (12 + 1) + (12 * 2) = 37

        # Overwriting formula 'c' with a constant value
        eng.set_value("c", 100)
        self.assertEqual(eng.evaluate("d"), 113)

    def test_cycle_detection_and_cycle_recovery(self) -> None:
        eng = ExpressionEngine()
        self.assertTrue(issubclass(CycleError, ValueError))

        # 3-node cycle: a -> b -> c -> a
        eng.set_formula("a", "b + 1")
        eng.set_formula("b", "c * 2")
        eng.set_formula("c", "a - 3")

        with self.assertRaises(CycleError):
            eng.evaluate("a")
        with self.assertRaises(CycleError):
            eng.evaluate("10 + b")

        # Self-cycle
        eng.set_formula("self_ref", "self_ref + 1")
        with self.assertRaises(CycleError):
            eng.evaluate("self_ref")

        # Breaking the 3-node cycle by setting 'c' to a constant
        eng.set_value("c", 4)
        self.assertEqual(eng.evaluate("a"), 9)  # b = 8, a = 9

    def test_error_handling_and_unsafe_rejection(self) -> None:
        eng = ExpressionEngine()
        self.assertTrue(issubclass(UnknownSymbolError, KeyError))

        with self.assertRaises(UnknownSymbolError):
            eng.evaluate("undefined_var * 2")

        with self.assertRaises(UnknownSymbolError):
            eng.evaluate("unknown_fn(5)")

        with self.assertRaises(ZeroDivisionError):
            eng.evaluate("10 / (3 - 3)")

        with self.assertRaises(ZeroDivisionError):
            eng.evaluate("10 // 0")

        with self.assertRaises(ZeroDivisionError):
            eng.evaluate("10 % 0")

        with self.assertRaises(TypeError):
            eng.set_value("flag", True)

        for bad_expr in ["1 + * 2", "__import__('os')", "(1 + 2", "[1, 2][0]", "'abc'"]:
            with self.assertRaises((SyntaxError, ValueError, UnknownSymbolError)):
                eng.set_formula("bad", bad_expr)
                eng.evaluate("bad")


if __name__ == "__main__":
    unittest.main()
