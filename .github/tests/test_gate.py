"""Exercise the gate's smoke prerequisite with isolated repo/tool fixtures."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


GATE = Path(__file__).resolve().parents[2] / "tools" / "gate.sh"


class SmokePrerequisiteTest(unittest.TestCase):
    def run_gate(self, checklist, smoke=None, go_status=0):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "tools/device-smoke/results").mkdir(parents=True)
            (root / "console").mkdir()
            (root / "cli").mkdir()
            (root / "bin").mkdir()
            shutil.copy2(GATE, root / "tools/gate.sh")
            (root / "CHECKLIST.md").write_text(checklist)
            if smoke is not None:
                (root / "tools/device-smoke/results/smoke.json").write_text(json.dumps(smoke))
            scripts = {
                "go": f"#!/bin/sh\nexit {go_status}\n",
                "bun": "#!/bin/sh\nexit 0\n",
                "git": """#!/bin/sh
case "$1" in
  merge-base) echo base;;
  diff) echo cli/runtime.go;;
  rev-parse) echo abc1234;;
  *) exit 1;;
esac
""",
            }
            for name, source in scripts.items():
                path = root / "bin" / name
                path.write_text(source)
                path.chmod(0o755)
            env = dict(os.environ, PATH=f"{root / 'bin'}:{os.environ['PATH']}")
            return subprocess.run(["bash", str(root / "tools/gate.sh")], env=env,
                                  capture_output=True, text=True, timeout=10)

    def test_bootstrap_does_not_require_device_smoke(self):
        result = self.run_gate("## S0. Groundwork\n| S0-03 | Units | [x] | — | — | PASS |\n"
                               "## F1. Install\n| F1-01 | Install | [!] | [ ] | [ ] | mentions [x] |\n")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("no feature checks already verified", result.stdout)

    def test_verified_scenario_requires_smoke(self):
        result = self.run_gate("## F1. Install\n| F1-01 | Install | [x] | [ ] | [ ] | PASS |\n")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("no all-PASS smoke result", result.stdout)

    def test_verified_command_requires_smoke(self):
        result = self.run_gate("## F2. Login\n| C001 | `pb login` | Login | [ ] | [x] | [ ] | PASS |\n")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("no all-PASS smoke result", result.stdout)

    def test_verified_feature_accepts_only_passing_head_evidence(self):
        checklist = "## F1. Install\n| F1-01 | Install | [x] | [x] | [x] | PASS |\n"
        for version, status, expected in [("dev-old", "PASS", 1),
                                           ("dev-abc1234", "FAIL", 1),
                                           ("dev-abc1234", "PASS", 0)]:
            with self.subTest(version=version, status=status):
                result = self.run_gate(checklist, {
                    "expect_version": version, "results": [{"status": status}],
                })
                self.assertEqual(result.returncode, expected, result.stdout + result.stderr)

    def test_bootstrap_still_rejects_failed_module(self):
        result = self.run_gate("## F1. Install\n| F1-01 | Install | [ ] | [ ] | [ ] | |\n", go_status=1)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("FAIL: cli", result.stdout)


if __name__ == "__main__":
    unittest.main()
