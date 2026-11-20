import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import host_resources
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

    def test_the_report_gives_each_resource_its_percentage_and_level_per_host(self):
        state, _ = assess({}, {"boot": "a", "cpu": [0, 0], "usage": {"memory": 85.04, "disk /": 10}}, 0)
        for minute in (1, 2, 3):
            state, _ = assess(state, {"boot": "a", "cpu": [0, 0], "usage": {"memory": 85.04, "disk /": 10}}, minute * 60)
        out = host_resources.report({"root@db": state, "bad@gone": {"misses": 3}})
        self.assertEqual(out["hosts"]["db"], {"resources": {"memory": {"percent": 85.0, "level": "warning"},
                                                            "disk /": {"percent": 10, "level": "normal"}},
                                              "unreachable": False})
        self.assertEqual(out["hosts"]["gone"], {"resources": {}, "unreachable": True})

    def test_a_host_that_cannot_be_sampled_alerts_once_and_recovers_once(self):
        sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "tools/probe"))
        import canary
        sample = {"boot": "a", "cpu": [0, 0], "usage": {"memory": 10}}
        results = [OSError("down")] * 4 + [sample]
        sent = []
        with tempfile.TemporaryDirectory() as directory, \
                patch.dict(os.environ, {"CANARY_RESOURCE_HOSTS": "h"}), \
                patch.object(canary, "STATE", Path(directory)), \
                patch.object(canary, "send_alert", sent.append), \
                patch.object(host_resources, "read_host", side_effect=results):
            for _ in results:
                host_resources.main()
        self.assertEqual(len(sent), 2)
        self.assertIn("UNKNOWN", sent[0])
        self.assertIn("RECOVERED", sent[1])


if __name__ == "__main__":
    unittest.main()
