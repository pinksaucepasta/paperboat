"""tools/ci-test.sh against fake git, gh and sleep: how long it waits for GitHub to list its run, and when it
may delete its ci branch (never while a run on it is queued or in progress)."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[2] / "tools" / "ci-test.sh"

FAKE_GH = r'''#!PYTHON
import json, os, sys
state_path = os.environ["FAKE_STATE"]
state = json.load(open(state_path))
args = sys.argv[1:]
with open(os.environ["FAKE_LOG"], "a") as log:
    log.write("gh " + " ".join(args) + "\n")
def value(flag):
    return args[args.index(flag) + 1] if flag in args else ""
if args[:2] == ["workflow", "run"]:
    sys.exit(0)
if args[:2] == ["run", "list"] and "databaseId" in value("--json"):
    state["lookups"] += 1
    json.dump(state, open(state_path, "w"))
    if state["appear_after"] and state["lookups"] >= state["appear_after"]:
        print(777)
    sys.exit(0)
if args[:2] == ["run", "list"] and value("--json") == "status":
    print(state["active"].get(value("--branch"), state["active_default"]))
    sys.exit(0)
if args[:2] == ["run", "view"]:
    fields = value("--json")
    if fields == "url":
        print("https://example.test/runs/777")
    elif fields == "status,jobs":
        print(json.dumps({"status": "completed", "jobs": []}))
    elif fields == "conclusion,jobs":
        print(json.dumps({"conclusion": "success", "jobs": []}))
    sys.exit(0)
sys.exit(1)
'''

FAKE_GIT = r'''#!/bin/sh
echo "git $*" >> "$FAKE_LOG"
case "$1" in
  rev-parse) if [ "$2" = --show-toplevel ]; then echo "$FAKE_ROOT"; else echo abc1234; fi ;;
  cat-file) exit 0 ;;
  status) ;;
  commit-tree) echo deadbeef ;;
  push) exit 0 ;;
  ls-remote) cat "$FAKE_REMOTE" ;;
  *) exit 1 ;;
esac
'''


class CITestScript(unittest.TestCase):
    def run_script(self, appear_after, active_default=0, active=None, remote_branches=()):
        directory = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, directory)
        root = Path(directory) / "lane1"
        (root / "tools").mkdir(parents=True)
        (root / "bin").mkdir()
        shutil.copy2(SCRIPT, root / "tools/ci-test.sh")
        state = root / "state.json"
        state.write_text(json.dumps({"lookups": 0, "appear_after": appear_after, "active_default": active_default,
                                     "active": active or {}}))
        remote = root / "remote"
        remote.write_text("".join(f"0000000 refs/heads/{name}\n" for name in remote_branches))
        log, sleeps = root / "calls.log", root / "sleeps.log"
        log.write_text("")
        sleeps.write_text("")
        for name, source in {"gh": FAKE_GH.replace("PYTHON", sys.executable), "git": FAKE_GIT,
                             "sleep": f'#!/bin/sh\necho "$1" >> "{sleeps}"\n'}.items():
            (root / "bin" / name).write_text(source)
            (root / "bin" / name).chmod(0o755)
        env = dict(os.environ, PATH=f"{root / 'bin'}{os.pathsep}{os.environ['PATH']}", FAKE_STATE=str(state),
                   FAKE_LOG=str(log), FAKE_ROOT=str(root), FAKE_REMOTE=str(remote))
        result = subprocess.run(["bash", str(root / "tools/ci-test.sh"), "smoke"], env=env, capture_output=True,
                                text=True, stdin=subprocess.DEVNULL)
        calls = log.read_text().splitlines()
        return result, calls, sleeps.read_text().split(), json.loads(state.read_text())["lookups"]

    @staticmethod
    def deleted(calls):
        return [call.split()[-1] for call in calls if call.startswith("git push") and "--delete" in call]

    def test_waits_past_the_old_90_s_for_a_slowly_listed_run_one_call_per_30_s(self):
        result, calls, sleeps, lookups = self.run_script(appear_after=6)  # 3 min: the old loop gave up at 90 s
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(lookups, 6)
        self.assertEqual(sleeps[:6], ["30"] * 6)
        self.assertEqual(len(self.deleted(calls)), 1, calls)  # its own branch, once the run is finished

    def test_gives_up_after_15_min_and_never_deletes_a_branch_with_a_queued_run(self):
        for active, deleted in ((1, 0), ("unknown", 0), (0, 1)):
            with self.subTest(active=active):
                result, calls, sleeps, lookups = self.run_script(appear_after=0, active_default=active)
                self.assertEqual(result.returncode, 1)
                self.assertIn("within 15 min", result.stderr)
                self.assertEqual((lookups, sleeps), (30, ["30"] * 30))
                self.assertEqual(len(self.deleted(calls)), deleted, calls)
                if not deleted:
                    self.assertIn("kept ci/lane1-", result.stderr)

    def test_deletes_only_its_own_branch_never_other_branches_of_the_lane(self):
        # 1fc37dd85: foreign ci/<lane>-* branches belong to concurrent runs, finished-looking or not.
        dead, queued, live = "ci/lane1-aaa-1-999999", "ci/lane1-bbb-1-999998", f"ci/lane1-ccc-1-{os.getpid()}"
        result, calls, _, _ = self.run_script(appear_after=1, active={queued: 1}, remote_branches=(dead, queued, live))
        self.assertEqual(result.returncode, 0, result.stderr)
        removed = self.deleted(calls)
        self.assertEqual(len(removed), 1, calls)
        self.assertRegex(removed[0], r"^ci/lane1-abc1234-\d{6}-\d+$")
        self.assertFalse(any(name in call for name in (dead, queued, live) for call in calls
                             if call.startswith("gh run list")), "a foreign branch was queried")


if __name__ == "__main__":
    unittest.main()
