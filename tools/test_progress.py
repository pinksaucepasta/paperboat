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


class CanaryLineTest(unittest.TestCase):
    def state(self, red, started="2026-11-11T10:00:00Z"):
        steps = [{"step": "exec_a_to_b", "ok": not red}, {"step": "tunnel", "ok": True}]
        run = {"started": started, "ok": not red, "steps": steps}
        return {"status": {"red": red, "red_since": started if red else None, "last": run}, "history": [run]}

    def now(self, minutes):
        return progress.ist("2026-11-11T10:00:00Z") + progress.timedelta(minutes=minutes)

    def test_single_total_cards_count_rows_on_any_platform(self):
        rows = [{"platforms": ["[x]", "[x]", "n/a"]}, {"platforms": ["n/a", "n/a", "[ ]"]},
                {"platforms": ["[x]", "[!]", "[ ]"]}, {"platforms": ["n/a", "—", "n/a"]}]
        self.assertEqual(progress.count_rows(rows), (1, 1, 3))

    def test_green_red_stale_and_missing(self):
        self.assertIn("green: 2/2 checks, last run 15:30 IST", progress.canary_line(self.state(False), self.now(1)))
        red = progress.canary_line(self.state(True), self.now(1))
        self.assertIn("RED since 15:30 IST: exec_a_to_b", red)
        self.assertIn('class="r" title="2026-11-11T10:00:00Z: exec_a_to_b"', red)
        self.assertIn("STALE: last run 15:30 IST", progress.canary_line(self.state(False), self.now(6)))
        self.assertIn("no result yet", progress.canary_line({"status": None, "history": []}))


if __name__ == "__main__":
    unittest.main()
