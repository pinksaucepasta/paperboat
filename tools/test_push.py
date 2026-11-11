"""Exercise the submission contract using real git refs and a fake GitHub API boundary."""
import json
import os
import subprocess
import tempfile
import time
import unittest
from pathlib import Path

SCRIPT = Path(__file__).with_name('push.sh').resolve()
FAKE_GH = '''#!/bin/sh
case "$1 $2" in
'repo view') echo test/repo ;;
'api repos/test/repo/branches/main/protection') echo "$PROTECTED" ;;
'api graphql') n=$(cat "$COUNTER" 2>/dev/null || echo 0); echo $((n + 1)) > "$COUNTER"
  # the first answer (right after submission) is armed; with LOST set every later one says the PR left the queue
  if [ -n "$LOST" ] && [ "$n" -gt 0 ]; then echo false; else echo true; fi ;;
'pr list') [ -z "$PR_HEAD" ] || echo 'https://example.test/pull/50' ;;
'pr create') echo "https://example.test/pull/${LANE_NUMBER:-1}" ;;
'pr merge') case "$*" in *--delete-branch*) echo 'merge queue refuses --delete-branch' >&2; exit 1 ;; esac ;;
'pr view') case "$*" in
  *headRefName*) echo "$PR_HEAD" ;;
  *statusCheckRollup*) echo "{\\"statusCheckRollup\\":[{\\"name\\":\\"Targeted Go checks\\",\\"conclusion\\":\\"$CHECK\\"},{\\"name\\":\\"End-to-end\\",\\"conclusion\\":\\"FAILURE\\"}]}" ;;
  *) if [ -n "$FLAG" ]; then [ -e "$FLAG" ] && echo MERGED || echo OPEN; else echo "$PR_STATE"; fi ;; esac ;;
*) exit 9 ;;
esac
'''


def git(*args, cwd, env):
    return subprocess.check_output(args, cwd=cwd, env=env, text=True, stderr=subprocess.DEVNULL).strip()


class Repo:
    """A bare origin and a lane clone on main with a protection file, one unsubmitted commit, and the fake gh on PATH."""

    def __init__(self, root):
        self.root = root
        self.env = dict(os.environ, GIT_CONFIG_NOSYSTEM='1', GIT_CONFIG_GLOBAL='/dev/null', PB_PUSH_POLL='0.2')
        git('git', 'init', '--bare', 'origin.git', cwd=root, env=self.env)
        git('git', 'init', '-b', 'main', 'lane', cwd=root, env=self.env)
        self.lane = root / 'lane'
        for key, value in (('user.email', 'test@example.test'), ('user.name', 'Test')):
            git('git', 'config', key, value, cwd=self.lane, env=self.env)
        (self.lane / 'tools').mkdir()
        (self.lane / 'tools' / 'branch-protection.json').write_text(
            json.dumps({'required_status_checks': {'checks': [{'context': 'Targeted Go checks'}]}}))
        git('git', 'add', '.', cwd=self.lane, env=self.env)
        git('git', 'commit', '-m', 'base', cwd=self.lane, env=self.env)
        self.original = git('git', 'rev-parse', 'HEAD', cwd=self.lane, env=self.env)
        git('git', 'remote', 'add', 'origin', str(root / 'origin.git'), cwd=self.lane, env=self.env)
        git('git', 'push', 'origin', 'main', cwd=self.lane, env=self.env)
        gh = root / 'gh'
        gh.write_text(FAKE_GH)
        gh.chmod(0o755)
        self.env.update(PATH=str(root) + ':' + self.env['PATH'], PROTECTED='true', CHECK='SUCCESS')

    def commit(self, cwd, message):
        git('git', 'commit', '--allow-empty', '-m', message, cwd=cwd, env=self.env)

    def refs(self):
        return git('git', '--git-dir', str(self.root / 'origin.git'), 'for-each-ref', '--format=%(refname)', cwd=self.root, env=self.env)

    def main_head(self):
        return git('git', '--git-dir', str(self.root / 'origin.git'), 'rev-parse', 'main', cwd=self.root, env=self.env)


class PushTests(unittest.TestCase):
    def run_case(self, state='MERGED', protected=True, check='SUCCESS', lost=False):
        with tempfile.TemporaryDirectory() as tmp:
            repo = Repo(Path(tmp))
            repo.commit(repo.lane, 'fix')
            env = dict(repo.env, PR_STATE=state, PROTECTED=str(protected).lower(), CHECK=check)
            env['COUNTER'] = str(Path(tmp) / 'counter')
            if lost:  # the queue dropped the PR: neither queued nor armed after submission
                env.update(LOST='1', PR_STATE='OPEN')
            done = subprocess.run(['sh', str(SCRIPT)], cwd=repo.lane, env=env, capture_output=True, text=True, timeout=20)
            self.assertEqual(repo.main_head(), repo.original)
            return done, repo.refs()

    def test_returns_success_only_when_merged_without_pushing_main(self):
        done, refs = self.run_case()
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertIn('refs/heads/lane-submit/', refs)

    def test_followup_keeps_published_history_when_main_advances(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = Repo(Path(tmp))
            branch = 'lane-submit/main/original-head'
            repo.commit(repo.lane, 'published fix')
            published = git('git', 'rev-parse', 'HEAD', cwd=repo.lane, env=repo.env)
            git('git', 'push', 'origin', f'HEAD:refs/heads/{branch}', cwd=repo.lane, env=repo.env)
            repo.commit(repo.lane, 'renumber row')
            head = git('git', 'rev-parse', 'HEAD', cwd=repo.lane, env=repo.env)
            tree = git('git', 'rev-parse', 'HEAD^{tree}', cwd=repo.lane, env=repo.env)
            upstream = git('git', 'commit-tree', tree, '-p', repo.original, '-m', 'other lane',
                           cwd=repo.lane, env=repo.env)
            git('git', 'push', 'origin', f'{upstream}:refs/heads/main', cwd=repo.lane, env=repo.env)
            env = dict(repo.env, PB_PUSH_PR='50', PR_HEAD=branch, PR_STATE='MERGED',
                       COUNTER=str(Path(tmp) / 'counter'))
            done = subprocess.run(['sh', str(SCRIPT)], cwd=repo.lane, env=env,
                                  capture_output=True, text=True, timeout=20)
            self.assertEqual(done.returncode, 0, done.stderr)
            remote_head = git('git', '--git-dir', str(repo.root / 'origin.git'),
                              'rev-parse', branch, cwd=repo.root, env=repo.env)
            for ancestor in (published, head, upstream):
                git('git', 'merge-base', '--is-ancestor', ancestor, remote_head,
                    cwd=repo.lane, env=repo.env)
            self.assertEqual(repo.main_head(), upstream)
            self.assertEqual(repo.refs().splitlines(), ['refs/heads/' + branch, 'refs/heads/main'])

    def test_failed_required_check_fails_the_push(self):
        done, _ = self.run_case(state='OPEN', check='FAILURE')
        self.assertNotEqual(done.returncode, 0)
        self.assertIn('CI failed', done.stderr)

    def test_failed_optional_check_does_not(self):
        # The fake always reports End-to-end FAILURE; with the required check green the push waits for the merge.
        done, _ = self.run_case(state='MERGED')
        self.assertEqual(done.returncode, 0, done.stderr)

    def test_pr_dropped_from_the_queue_fails_instead_of_waiting_an_hour(self):
        done, _ = self.run_case(state='OPEN', lost=True)
        self.assertNotEqual(done.returncode, 0)
        self.assertIn('left the merge queue', done.stderr)

    def test_closed_pr_fails(self):
        done, _ = self.run_case(state='CLOSED')
        self.assertNotEqual(done.returncode, 0)
        self.assertIn('closed without merging', done.stderr)

    def test_unprotected_base_refuses_before_any_push(self):
        done, refs = self.run_case(protected=False)
        self.assertNotEqual(done.returncode, 0)
        self.assertNotIn('lane-submit', refs)


class ConcurrencyTest(unittest.TestCase):
    def test_second_lane_submits_while_the_first_still_waits_for_its_merge(self):
        """The shared lock covers only the submit step: lane B opens its PR while lane A's PR is still unmerged."""
        with tempfile.TemporaryDirectory() as tmp:
            repo = Repo(Path(tmp))
            lane_b = Path(tmp) / 'lane-b'
            git('git', 'worktree', 'add', '-b', 'lane-b', str(lane_b), 'main', cwd=repo.lane, env=repo.env)
            repo.commit(repo.lane, 'fix a')
            repo.commit(lane_b, 'fix b')
            flag = Path(tmp) / 'merged'
            procs = {}
            for name, cwd, number in (('a', repo.lane, '1'), ('b', lane_b, '2')):
                out = open(Path(tmp) / f'{name}.err', 'w')
                env = dict(repo.env, FLAG=str(flag), LANE_NUMBER=number, COUNTER=str(Path(tmp) / f'counter-{name}'))
                procs[name] = (subprocess.Popen(['sh', str(SCRIPT)], cwd=cwd, env=env, stdout=out, stderr=subprocess.STDOUT), out)
            deadline = time.monotonic() + 15
            submitted = set()
            while time.monotonic() < deadline and len(submitted) < 2:
                for name in ('a', 'b'):
                    if 'push: PR ' in (Path(tmp) / f'{name}.err').read_text():
                        submitted.add(name)
                time.sleep(0.1)
            alive = {name: proc.poll() is None for name, (proc, _) in procs.items()}
            flag.write_text('merged')  # the queue merges both
            codes = {name: proc.wait(timeout=20) for name, (proc, _) in procs.items()}
            self.assertEqual(submitted, {'a', 'b'}, 'the second lane was blocked behind the first one\'s merge')
            self.assertEqual(alive, {'a': True, 'b': True}, 'both still wait for their merge, outside the lock')
            self.assertEqual(codes, {'a': 0, 'b': 0})


if __name__ == '__main__':
    unittest.main()
