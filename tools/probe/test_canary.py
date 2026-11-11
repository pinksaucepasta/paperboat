#!/usr/bin/env python3
"""Tests for canary.py: result parsing, bounded history, and alert-once / recover-once per step."""
import json, os, sys, tempfile, unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import canary  # noqa: E402

OK = '{"step":"exec_a_to_b","ok":true,"seconds":1}'
BAD = '{"step":"exec_a_to_b","ok":false,"seconds":45,"error":"machine unreachable"}'


def run(lines, rc, started="2026-11-11T10:00:00Z"):
    steps = canary.parse("\n".join(lines), rc)
    return {"started": started, "finished": started, "seconds": 1, "ok": all(s["ok"] for s in steps), "steps": steps}


class Parse(unittest.TestCase):
    def test_steps_and_noise(self):
        steps = canary.parse("ssh: warning\n" + OK + "\n" + BAD + "\n[1, 2]\n", 1)
        self.assertEqual([(s["step"], s["ok"]) for s in steps], [("exec_a_to_b", True), ("exec_a_to_b", False)])
        self.assertEqual(steps[1]["error"], "machine unreachable")

    def test_no_output_is_a_failure(self):
        self.assertEqual(canary.parse("", 124), [{"step": "probe", "ok": False, "seconds": 0,
                                                    "error": "probe printed no result (exit 124)"}])

    def test_nonzero_exit_with_only_passes_is_a_failure(self):
        self.assertFalse(run([OK], 2)["ok"])

    def test_wrong_types_are_ignored(self):
        self.assertEqual(canary.parse('{"step":"x","ok":"true"}', 0)[0]["step"], "probe")


class Record(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        canary.STATE = Path(self.dir.name)
        self.sent = []

    def tearDown(self):
        self.dir.cleanup()

    def record(self, lines, rc, started="2026-11-11T10:00:00Z"):
        return canary.record(run(lines, rc, started), self.sent.append)

    def test_alert_after_two_failures_once_then_recovery_once(self):
        self.record([OK], 0)
        status = self.record([BAD], 1, "2026-11-11T10:01:00Z")
        self.assertEqual(self.sent, [])
        self.assertTrue(status["red"])
        self.assertEqual(status["red_since"], "2026-11-11T10:01:00Z")
        status = self.record([BAD], 1, "2026-11-11T10:02:00Z")
        self.assertEqual(len(self.sent), 1)
        self.assertIn("canary RED: exec_a_to_b failed 2 runs in a row", self.sent[0])
        self.assertEqual(status["red_since"], "2026-11-11T10:01:00Z")
        self.record([BAD], 1)
        self.assertEqual(len(self.sent), 1)
        status = self.record([OK], 0)
        self.assertFalse(status["red"])
        self.assertIsNone(status["red_since"])
        self.assertEqual(len(self.sent), 2)
        self.assertIn("RECOVERED", self.sent[1])
        self.assertEqual((canary.STATE / "alerts.log").read_text().count("\n"), 2)

    def test_steps_failing_together_send_one_message(self):
        tunnel_bad = '{"step":"tunnel","ok":false,"seconds":1,"error":"404"}'
        self.record([BAD, tunnel_bad], 1)
        self.record([BAD, tunnel_bad], 1)
        self.assertEqual(len(self.sent), 1)
        self.assertEqual(self.sent[0].count("canary RED"), 2)

    def test_a_single_failure_between_passes_never_alerts(self):
        for lines in ([BAD], [OK], [BAD], [OK]):
            self.record(lines, 0 if lines == [OK] else 1)
        self.assertEqual(self.sent, [])

    def test_history_is_bounded(self):
        canary.HISTORY_MAX = 3
        try:
            for minute in range(5):
                self.record([OK], 0, f"2026-11-11T10:0{minute}:00Z")
        finally:
            canary.HISTORY_MAX = 1440
        lines = (canary.STATE / "history.jsonl").read_text().splitlines()
        self.assertEqual([json.loads(l)["started"] for l in lines],
                         ["2026-11-11T10:02:00Z", "2026-11-11T10:03:00Z", "2026-11-11T10:04:00Z"])


if __name__ == "__main__":
    unittest.main()
