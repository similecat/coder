import unittest

import codec


class CodecSmokeTests(unittest.TestCase):
    def test_module_has_entrypoints(self) -> None:
        self.assertTrue(callable(codec.encode_packet))
        self.assertTrue(callable(codec.decode_packet))


if __name__ == "__main__":
    unittest.main()
