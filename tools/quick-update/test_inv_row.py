import json, subprocess, sys, tempfile, unittest
from pathlib import Path

ROW = Path(__file__).with_name("inv_row.py")


def row(*lines):
    with tempfile.NamedTemporaryFile("w", suffix=".json") as f:
        f.write("\n".join(lines) + "\n")
        f.flush()
        done = subprocess.run([sys.executable, str(ROW), "t", f.name], capture_output=True, text=True)
    return done.returncode, done.stdout


class InvRowTest(unittest.TestCase):
    def test_empty_remaining_passes(self):
        code, out = row(json.dumps({"ok": True, "remaining": []}))
        self.assertEqual(code, 0)
        self.assertIn("PASS", out)

    def test_leftover_fails_and_is_named(self):
        code, out = row(json.dumps({"ok": False, "remaining": ["process 7 (/x/pb) runs a replaced image"]}))
        self.assertEqual(code, 1)
        self.assertIn("process 7", out)

    def test_controller_container_processes_are_not_the_targets(self):
        code, out = row(json.dumps({"ok": False, "remaining": ["process 9 (/x/pb) runs a replaced image"]}),
                        json.dumps({"container_pids": [9]}))
        self.assertEqual(code, 0)
        self.assertIn("ignored 1", out)

    def test_a_check_that_did_not_run_fails(self):
        code, out = row("pb: something broke")
        self.assertEqual(code, 1)
        self.assertIn("did not run", out)

    def test_a_build_without_the_inventory_command_is_skipped_visibly(self):
        code, out = row("pb: cause: service requires install, commit, repair, repair-persisted, stop, or uninstall")
        self.assertEqual(code, 0)
        self.assertIn("SKIPPED", out)


if __name__ == "__main__":
    unittest.main()
