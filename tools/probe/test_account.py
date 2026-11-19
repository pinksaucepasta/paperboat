#!/usr/bin/env python3
"""Tests for account.py: one JSON line per step, cleanup and sign-out run whatever happened, nothing runs after a failed sign-in."""
import io, json, os, sys, unittest
from contextlib import redirect_stdout
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import account  # noqa: E402

ENV = {"ACCOUNT_EMAIL": "x@example.test", "ACCOUNT_PASSWORD_FILE": "/nonexistent", "ACCOUNT_ORIGIN": "https://console.example.test",
       "ACCOUNT_RUN": "true", "ACCOUNT_SERVER": "https://api.example.test"}


def run_main(node=None, pair=None, login=None, cleanup=None):
    out = io.StringIO()
    ok = lambda *a, **k: None
    with mock.patch.dict(os.environ, ENV), mock.patch.object(account, "node", node or ok), mock.patch.object(account, "pair", pair or ok), \
            mock.patch.object(account, "login", login or ok), mock.patch.object(account, "cleanup", cleanup or ok), redirect_stdout(out):
        rc = account.main()
    return rc, [json.loads(line) for line in out.getvalue().splitlines()]


class Main(unittest.TestCase):
    def test_all_green_reports_every_step(self):
        rc, steps = run_main()
        self.assertEqual((rc, [s["step"] for s in steps]), (0, ["signin", "pair", "login", "cleanup", "signout"]))
        self.assertTrue(all(s["ok"] for s in steps))

    def test_failed_signin_stops_everything_after_it(self):
        def node(*args, **kwargs):
            raise RuntimeError("console_session.mjs failed: no sign-in")
        rc, steps = run_main(node=node)
        self.assertEqual((rc, [(s["step"], s["ok"]) for s in steps]), (1, [("signin", False)]))
        self.assertIn("no sign-in", steps[0]["error"])

    def test_failed_pair_skips_login_but_still_cleans_up_and_signs_out(self):
        def pair(state):
            raise RuntimeError("pb pair exited 1")
        rc, steps = run_main(pair=pair)
        self.assertEqual(rc, 1)
        self.assertEqual([(s["step"], s["ok"]) for s in steps], [("signin", True), ("pair", False), ("cleanup", True), ("signout", True)])

    def test_failed_login_still_cleans_up(self):
        def login():
            raise RuntimeError("auth status is not signed in")
        rc, steps = run_main(login=login)
        self.assertEqual([(s["step"], s["ok"]) for s in steps], [("signin", True), ("pair", True), ("login", False), ("cleanup", True), ("signout", True)])


class RunPair(unittest.TestCase):
    def pair(self, results):
        with mock.patch.object(account, "pb", side_effect=results) as pb, mock.patch.object(account, "wait_pair_host") as wait:
            return account.run_pair("canary-pair-1"), pb.call_count, wait.call_count

    def test_container_killed_under_pair_waits_for_the_host_and_pairs_again(self):
        self.assertEqual(self.pair([(137, {}), (0, {"ok": True})]), ((0, {"ok": True}), 2, 1))

    def test_a_second_kill_is_reported_not_retried_again(self):
        self.assertEqual(self.pair([(137, {}), (137, {})]), ((137, {}), 2, 1))

    def test_an_ordinary_failure_is_not_retried(self):
        self.assertEqual(self.pair([(1, {"ok": False})]), ((1, {"ok": False}), 1, 0))


class WaitPairHost(unittest.TestCase):
    def wait(self, stdouts, seconds=90):
        done = [mock.Mock(stdout=out) for out in stdouts]
        with mock.patch.dict(os.environ, ENV), mock.patch.object(account.subprocess, "run", side_effect=done), mock.patch.object(account.time, "sleep"):
            account.wait_pair_host(seconds)

    def test_returns_once_the_user_manager_runs(self):
        self.wait(["", "starting\n", "running\n"])

    def test_gives_up_with_a_named_error(self):
        with mock.patch.object(account.time, "monotonic", side_effect=[0, 1, 100]):
            with self.assertRaisesRegex(RuntimeError, "did not come back"):
                self.wait([""])


if __name__ == "__main__":
    unittest.main()
