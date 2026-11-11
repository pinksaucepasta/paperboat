"""Exercise the submission contract using real git refs and a fake GitHub API boundary."""
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).with_name('push.sh').resolve()


class PushTests(unittest.TestCase):
    def run_case(self, state='MERGED', protected=True):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            env = dict(os.environ, GIT_CONFIG_NOSYSTEM='1', GIT_CONFIG_GLOBAL='/dev/null')
            def run(*args, cwd=root):
                return subprocess.check_output(args, cwd=cwd, env=env, text=True, stderr=subprocess.DEVNULL).strip()
            run('git', 'init', '--bare', 'origin.git')
            run('git', 'init', '-b', 'main', 'lane')
            lane = root / 'lane'
            run('git', 'config', 'user.email', 'test@example.test', cwd=lane)
            run('git', 'config', 'user.name', 'Test', cwd=lane)
            run('git', 'commit', '--allow-empty', '-m', 'base', cwd=lane)
            original = run('git', 'rev-parse', 'HEAD', cwd=lane)
            run('git', 'remote', 'add', 'origin', str(root / 'origin.git'), cwd=lane)
            run('git', 'push', 'origin', 'main', cwd=lane)
            run('git', 'commit', '--allow-empty', '-m', 'fix', cwd=lane)
            gh = root / 'gh'
            gh.write_text('''#!/bin/sh
case "$1 $2" in
'repo view') echo test/repo ;;
'api repos/test/repo/branches/main/protection') echo "$PROTECTED" ;;
'pr list') ;;
'pr create') echo https://example.test/pull/1 ;;
'pr merge') case "$*" in *--delete-branch*) echo 'merge queue refuses --delete-branch' >&2; exit 1 ;; esac ;;
'pr view') case "$*" in *statusCheckRollup*) echo true ;; *) echo "$PR_STATE" ;; esac ;;
*) exit 9 ;;
esac
''')
            gh.chmod(0o755)
            env.update(PATH=str(root) + ':' + env['PATH'], PR_STATE=state, PROTECTED=str(protected).lower())
            done = subprocess.run(['sh', str(SCRIPT)], cwd=lane, env=env, capture_output=True, text=True, timeout=10)
            self.assertEqual(run('git', '--git-dir', str(root / 'origin.git'), 'rev-parse', 'main'), original)
            refs = run('git', '--git-dir', str(root / 'origin.git'), 'for-each-ref', '--format=%(refname)')
            return done, refs

    def test_returns_success_only_when_merged_without_pushing_main(self):
        done, refs = self.run_case()
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertIn('refs/heads/lane-submit/', refs)

    def test_failed_ci_releases_the_queue_without_reporting_success(self):
        done, _ = self.run_case(state='OPEN')
        self.assertNotEqual(done.returncode, 0)
        self.assertIn('CI failed', done.stderr)

    def test_closed_pr_fails(self):
        done, _ = self.run_case(state='CLOSED')
        self.assertNotEqual(done.returncode, 0)
        self.assertIn('closed without merging', done.stderr)

    def test_unprotected_base_refuses_before_any_push(self):
        done, refs = self.run_case(protected=False)
        self.assertNotEqual(done.returncode, 0)
        self.assertNotIn('lane-submit', refs)
