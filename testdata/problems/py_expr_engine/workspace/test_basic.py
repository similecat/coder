import unittest

from expr_engine import CycleError, ExpressionEngine, UnknownSymbolError


class BasicExpressionTests(unittest.TestCase):
    def test_arithmetic_and_variables(self) -> None:
        eng = ExpressionEngine()
        self.assertEqual(eng.evaluate("2 + 3 * 4"), 14)
        self.assertEqual(eng.evaluate("2 ** 3 ** 2"), 512)

        eng.set_value("a", 5)
        eng.set_formula("b", "a * 2 + 1")
        self.assertEqual(eng.evaluate("b"), 11)

        eng.set_value("a", 10)
        self.assertEqual(eng.evaluate("b"), 21)

    def test_errors(self) -> None:
        eng = ExpressionEngine()
        with self.assertRaises(UnknownSymbolError):
            eng.evaluate("missing_var + 1")

        eng.set_formula("x", "y + 1")
        eng.set_formula("y", "x + 1")
        with self.assertRaises(CycleError):
            eng.evaluate("x")


if __name__ == "__main__":
    unittest.main()
