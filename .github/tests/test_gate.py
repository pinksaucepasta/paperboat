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
    def run_gate(self, checklist, smoke=None, go_status=0, history=(), runtime_changed=True):
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
            for index, previous in enumerate(history):
                (root / f"tools/device-smoke/results/previous-{index}.json").write_text(json.dumps(previous))
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
            if not runtime_changed:
                scripts["git"] = scripts["git"].replace("echo cli/runtime.go", "echo .github/workflows/release.yml")
                (root / "bin/git").write_text(scripts["git"])
            env = dict(os.environ, PATH=f"{root / 'bin'}:{os.environ['PATH']}")
            return subprocess.run(["bash", str(root / "tools/gate.sh")], env=env,
                                  capture_output=True, text=True, timeout=10)

    def test_runtime_push_requires_head_smoke_independent_of_checklist(self):
        for cells in ["[ ] | [ ] | [ ]", "[!] | [ ] | [ ]", "[x] | [x] | [x]"]:
            with self.subTest(cells=cells):
                result = self.run_gate(f"## F1. Install\n| F1-01 | Install | {cells} | |\n")
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("no smoke result for build dev-abc1234", result.stdout)

    def test_every_historical_pass_must_pass_on_head(self):
        history = [
            {"expect_version": "dev-older", "results": [{"id": "ssh", "status": "PASS"}]},
            {"expect_version": "dev-old", "results": [{"id": "exec", "status": "PASS"},
                                                       {"id": "ssh", "status": "FAIL"}]},
        ]
        for version, results, expected in [
            ("dev-wrong", [{"id": "ssh", "status": "PASS"}, {"id": "exec", "status": "PASS"}], 1),
            ("dev-abc1234", [{"id": "exec", "status": "PASS"}], 1),
            ("dev-abc1234", [{"id": "ssh", "status": "FAIL"}, {"id": "exec", "status": "PASS"}], 1),
            ("dev-abc1234", [{"id": "ssh", "status": "PASS"}, {"id": "exec", "status": "PASS"},
                             {"id": "new-check", "status": "FAIL"}], 0),
        ]:
            with self.subTest(version=version, results=results):
                result = self.run_gate("", {"expect_version": version, "results": results}, history=history)
                self.assertEqual(result.returncode, expected, result.stdout + result.stderr)

    def test_nonruntime_changes_do_not_require_device_smoke(self):
        result = self.run_gate("", runtime_changed=False)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_same_head_previous_pass_cannot_regress(self):
        history = [{"expect_version": "dev-abc1234", "results": [
            {"id": "same-build", "status": "PASS"}]}]
        for results in [[], [{"id": "same-build", "status": "FAIL"}]]:
            with self.subTest(results=results):
                result = self.run_gate("", {"expect_version": "dev-abc1234", "results": results},
                                       history=history)
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_module_failure_blocks_push_with_passing_smoke(self):
        result = self.run_gate("", {"expect_version": "dev-abc1234", "results": []}, go_status=1)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("FAIL: cli", result.stdout)


if __name__ == "__main__":
    unittest.main()
