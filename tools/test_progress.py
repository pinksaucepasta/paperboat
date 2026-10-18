"""Platform counting contract for the published checklist dashboard."""
import unittest

import progress


class PlatformProgressTest(unittest.TestCase):
    def test_each_platform_counts_its_own_cells(self):
        sections = progress.parse("""## F7. Previews
| F7-01 | create | [x] | [!] | [~] | receipt |
| F7-02 | list | [x] | [x] | n/a | receipt |
| C082 | pb preview | expose | [x] | [ ] | [ ] | receipt |
""")
        rows = sections[0]["feature"] + sections[0]["command"]
        self.assertEqual(progress.count(rows, 0), (3, 0, 3))
        self.assertEqual(progress.count(rows, 1), (1, 1, 3))
        self.assertEqual(progress.count(rows, 2), (0, 0, 2))


if __name__ == "__main__":
    unittest.main()
