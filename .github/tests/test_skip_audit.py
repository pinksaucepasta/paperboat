"""tools/skip-audit.py: a test that skipped in every CI step and ran in none fails the run (no silent skips)."""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
AUDIT = ROOT / "tools/skip-audit.py"
REPORT = ROOT / "tools/gotest-json.py"
PERF = ("github.com/pinksaucepasta/paperboat/server/internal/app", "TestPerformanceBudgetsOnSeededPostgres",
        "set PAPERBOAT_PERF_DATABASE_DSN to an isolated *_test database to run the performance budgets")
ROOT_TEST = ("github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostinstall",
             "TestInstallLockRecreatesResetStateWithoutRepairingUnsafeEntries", "protected native installer state requires root")


def events(package, test, action, reason=""):
    out = [{"Action": "run", "Package": package, "Test": test},
           {"Action": "output", "Package": package, "Test": test, "Output": "=== RUN   %s\n" % test}]
    if reason:
        out.append({"Action": "output", "Package": package, "Test": test, "Output": "    x_test.go:12: %s\n" % reason})
    out.append({"Action": "output", "Package": package, "Test": test, "Output": "--- %s: %s (0.00s)\n" % (action.upper(), test)})
    out.append({"Action": action, "Package": package, "Test": test})
    return out


class SkipAuditTest(unittest.TestCase):
    def run_audit(self, steps, allowlist="", baseline=""):
        with tempfile.TemporaryDirectory() as directory:
            files = []
            for name, rows in steps.items():
                path = Path(directory) / (name + ".jsonl")
                path.write_text("".join(json.dumps(r) + "\n" for r in rows))
                files.append(str(path))
            allow, base = Path(directory) / "allow.txt", Path(directory) / "baseline.txt"
            allow.write_text(allowlist)
            base.write_text(baseline)
            return subprocess.run([sys.executable, str(AUDIT), str(allow), str(base), *files], capture_output=True, text=True)

    def test_the_perf_budgets_before_their_ci_step_are_a_silent_skip(self):
        # Before the lane-test perf step (F21-15) the test skipped in the only step that saw it.
        result = self.run_audit({"main": events(*PERF[:2], "skip", PERF[2])})
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertIn("SILENT SKIP %s %s: x_test.go:12: %s" % PERF, result.stdout)
        # With the step it runs there, and the skip in the main step is fine.
        result = self.run_audit({"main": events(*PERF[:2], "skip", PERF[2]), "perf": events(*PERF[:2], "pass")})
        self.assertEqual(result.returncode, 0, result.stdout)

    def test_root_tests_before_the_sudo_step_are_a_silent_skip(self):
        # Before 099483ba4 the cli root tests ran only as the unprivileged runner user.
        result = self.run_audit({"main": events(*ROOT_TEST[:2], "skip", ROOT_TEST[2])})
        self.assertEqual(result.returncode, 1)
        self.assertIn(ROOT_TEST[1], result.stdout)
        result = self.run_audit({"main": events(*ROOT_TEST[:2], "skip", ROOT_TEST[2]),
                                 "root": events(*ROOT_TEST[:2], "fail")})
        self.assertEqual(result.returncode, 0, "a test that ran and failed is not a silent skip; go test fails it")

    def test_the_baseline_is_a_ratchet(self):
        known = "%s %s\n" % PERF[:2]
        skipped_everywhere = {"main": events(*PERF[:2], "skip", PERF[2])}
        self.assertEqual(self.run_audit(skipped_everywhere, baseline=known).returncode, 0, "a baselined skip passes")
        new = {"main": events(*PERF[:2], "skip", PERF[2]) + events(*ROOT_TEST[:2], "skip", ROOT_TEST[2])}
        result = self.run_audit(new, baseline=known)
        self.assertEqual(result.returncode, 1, "a new never-run test fails")
        self.assertIn("SILENT SKIP " + ROOT_TEST[0], result.stdout)
        result = self.run_audit({"main": events(*PERF[:2], "skip", PERF[2]), "perf": events(*PERF[:2], "pass")}, baseline=known)
        self.assertEqual(result.returncode, 1, "a baselined test that runs now must leave the baseline")
        self.assertIn("BASELINE STALE %s %s" % PERF[:2], result.stdout)

    def test_an_allowlist_line_must_name_where_the_test_runs(self):
        rows = {"main": events("pkg", "TestOnlyOnWindows", "skip", "only on windows")}
        self.assertEqual(self.run_audit(rows, "only on windows\tcross-platform.yml\n").returncode, 0)
        result = self.run_audit(rows, "only on windows\n")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("needs REGEX<TAB>the job", result.stderr + result.stdout)

    def test_the_report_prints_what_plain_go_test_prints_and_saves_every_event(self):
        rows = events("pkg", "TestGood", "pass") + events("pkg", "TestBad", "fail", "boom") + [
            {"Action": "output", "Package": "pkg", "Output": "FAIL\tpkg\t0.01s\n"}]
        with tempfile.TemporaryDirectory() as directory:
            saved = Path(directory) / "events.jsonl"
            stdin = "".join(json.dumps(r) + "\n" for r in rows)
            quiet = subprocess.run([sys.executable, str(REPORT), "--save", str(saved)], input=stdin,
                                   capture_output=True, text=True, check=True).stdout
            self.assertIn("--- FAIL: TestBad", quiet)
            self.assertIn("boom", quiet)
            self.assertNotIn("TestGood", quiet)
            self.assertIn("FAIL\tpkg", quiet)
            self.assertEqual(len(saved.read_text().splitlines()), len(rows))
            verbose = subprocess.run([sys.executable, str(REPORT), "--save", str(saved), "--verbose"], input=stdin,
                                     capture_output=True, text=True, check=True).stdout
            self.assertIn("--- PASS: TestGood", verbose)

    def test_a_timed_out_package_prints_the_test_that_was_still_running(self):
        # go test -timeout: the running test never gets a fail event, only its package does.
        rows = [{"Action": "run", "Package": "pkg", "Test": "TestHangs"},
                {"Action": "output", "Package": "pkg", "Test": "TestHangs", "Output": "=== RUN   TestHangs\n"},
                {"Action": "output", "Package": "pkg", "Test": "TestHangs", "Output": "panic: test timed out after 30m0s\n"},
                {"Action": "output", "Package": "pkg", "Output": "FAIL\tpkg\t1800.1s\n"},
                {"Action": "fail", "Package": "pkg"}]
        with tempfile.TemporaryDirectory() as directory:
            stdin = "".join(json.dumps(r) + "\n" for r in rows)
            out = subprocess.run([sys.executable, str(REPORT), "--save", str(Path(directory) / "e.jsonl")], input=stdin,
                                 capture_output=True, text=True, check=True).stdout
        self.assertIn("panic: test timed out", out)
        self.assertIn("=== RUN   TestHangs", out)


if __name__ == "__main__":
    unittest.main()
