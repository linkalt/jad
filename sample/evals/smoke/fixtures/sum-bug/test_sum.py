import unittest

from sum import total


class TotalTest(unittest.TestCase):
    def test_total(self):
        self.assertEqual(total([1, 2, 3]), 6)

    def test_empty(self):
        self.assertEqual(total([]), 0)


if __name__ == "__main__":
    unittest.main()
