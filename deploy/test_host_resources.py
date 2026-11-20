import unittest

from host_resources import assess


class HostResourceTest(unittest.TestCase):
    def test_sustained_alert_escalation_hysteresis_and_recovery(self):
        state = {}
        cases = (
            (81, []), (92, []), (81, []), (92, ["WARNING: memory at 92.0%"]),
            (92, []), (92, []), (92, ["CRITICAL: memory at 92.0%"]),
            (89, []), (86, []), (84, ["WARNING: memory at 84.0%"]),
            (79, []), (74, ["RECOVERED: memory at 74.0%"]), (81, []),
        )
        for minute, (percent, expected) in enumerate(cases):
            with self.subTest(minute=minute):
                state, alerts = assess(state, {"boot": "a", "cpu": [0, 0],
                                               "usage": {"memory": percent}}, minute * 60)
                self.assertEqual(alerts, expected)

    def test_cpu_uses_counter_deltas_and_restarts_after_gap_or_reboot(self):
        state = {}
        for minute, seconds in enumerate((0, 60, 120, 300, 360, 420, 480, 540, 600)):
            with self.subTest(seconds=seconds):
                # Cumulative idle is high. Every complete interval is 95% busy.
                state, alerts = assess(state, {"boot": "a", "cpu": [1000 + 100 * minute, 850 + 5 * minute],
                                               "usage": {}}, seconds)
                self.assertEqual(alerts, ["CRITICAL: CPU at 95.0%"] if seconds == 540 else [])
        state, alerts = assess(state, {"boot": "b", "cpu": [2, 2], "usage": {}}, 660)
        self.assertEqual(alerts, [])
        _, alerts = assess(state, {"boot": "b", "cpu": [102, 102], "usage": {}}, 720)
        self.assertEqual(alerts, ["RECOVERED: CPU at 0.0%"])


if __name__ == "__main__":
    unittest.main()
