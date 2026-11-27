"""Launcher and fixture rules: a cut that would kill the client is refused, a teammate counts only when the server
answers it, a failed row names its failed steps, and the host cut refuses a bad request before it connects."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import windows_checks

HERE = Path(__file__).resolve().parent

# A pb whose `auth status` says signed in while the server answers `team list` only after a token sign-in.
PB = '''#!/bin/sh
case "$*" in
"config show --json") echo '{"data":{"server_url":"https://api.example.test"}}';;
*"auth status --json") echo '{"signed_in":true}';;
*"team list --json") [ -f "$HOME_OF_TEST/live" ] || exit 1; echo '[{"team_id":"paid"}]';;
*"login --api-token -") cat > "$HOME_OF_TEST/live";;
*) exit 9;;
esac
'''


class Teammate(unittest.TestCase):
    def login(self, home, token, *arguments):
        env = dict(os.environ, HOME=str(home), HOME_OF_TEST=str(home))
        return subprocess.run([sys.executable, '-B', str(HERE / 'teammate_login.py'), *arguments], input=token,
                              capture_output=True, text=True, env=env, timeout=60)

    @unittest.skipUnless(os.uname().sysname == 'Linux', 'the profile paths are the Linux ones')
    def test_an_expired_session_is_signed_in_again_and_the_team_must_list_it(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            (home / '.local/bin').mkdir(parents=True)
            (home / '.local/bin/pb').write_text(PB)
            (home / '.local/bin/pb').chmod(0o755)
            self.assertNotIn('signed-in', self.login(home, '').stdout)  # empty token: nothing live to keep
            (home / 'live').unlink(missing_ok=True)
            done = self.login(home, 'pbat_token', 'paid')
            self.assertEqual((done.returncode, (home / 'live').read_text()), (0, 'pbat_token'), done.stderr)
            self.assertEqual(self.login(home, '', 'paid').stdout, 'signed-in\n')
            done = self.login(home, '', 'other-team')
            self.assertEqual(done.returncode, 3)
            self.assertIn('NOT an ACTIVE member', done.stderr)

    def test_nothing_is_removed_from_the_team_before_the_teammate_session_is_proved(self):
        import team_lifecycle

        def dead(*arguments, teammate=False):
            raise RuntimeError('401')

        with self.assertRaisesRegex(RuntimeError, 'NOT LIVE: nothing was removed'):
            team_lifecycle.prove_teammate_live(dead, 'paid', True)
        with self.assertRaisesRegex(RuntimeError, 'DOES NOT LIST paid'):
            team_lifecycle.prove_teammate_live(lambda *a, **k: [], 'paid', True)
        team_lifecycle.prove_teammate_live(lambda *a, **k: [], 'paid', False)
        team_lifecycle.prove_teammate_live(lambda *a, **k: [{'team_id': 'paid'}], 'paid', True)
        text = (HERE / 'team_lifecycle.py').read_text()
        self.assertLess(text.index('prove_teammate_live(call, team_id, was_member)'), text.index("call('team', 'remove', team_id,"))


class FailureDetail(unittest.TestCase):
    def test_the_failed_steps_come_first_however_long_the_output_after_them_is(self):
        output = 'PASS a\nFAIL N5 server unreachable: refused\n' + 'SKIP N13 black hole: needs a firewall ' + 'x' * 900
        detail = windows_checks.failure_detail(1, output, '')
        self.assertTrue(detail.startswith('exit 1: FAIL N5 server unreachable: refused'), detail)
        self.assertNotIn('SKIP', detail)
        record = json.dumps({'check': 'path-display', 'status': 'FAIL', 'samples': [1] * 500, 'detail': 'wrong path'})
        self.assertEqual(windows_checks.failure_detail(1, record, ''), 'exit 1: FAIL path-display: wrong path')
        summary = 'FAIL F5-07 setup slow\n' + json.dumps({'check': 'ssh-mac-rows', 'status': 'FAIL', 'results': []})
        self.assertEqual(windows_checks.failure_detail(1, summary, ''), 'exit 1: FAIL F5-07 setup slow')
        self.assertIn('RESULT FAIL roles', windows_checks.failure_detail(1, 'RESULT FAIL roles\nRESULT PASS x', ''))
        self.assertEqual(windows_checks.failure_detail(2, 'output', 'Traceback: boom'), 'exit 2: Traceback: boom')


class Timeouts(unittest.TestCase):
    def test_a_long_hold_is_not_cut_by_the_common_limit(self):
        argv = ['ssh_long.py', 'peer', '--interactive', '--seconds', '3601']
        self.assertEqual(windows_checks.check_timeout('ssh-long-interactive', argv, 1800), 4201)
        self.assertEqual(windows_checks.check_timeout('exec-long', ['exec_long.py', 'p', '--seconds', ''], 1800), 1800)
        self.assertEqual(windows_checks.check_timeout('ssh-main', ['ssh_lifecycle.py', 'peer'], 1800), 1800)


class Cuts(unittest.TestCase):
    def test_n5_refuses_instead_of_the_cut_that_stops_the_windows_client(self):
        import terminal_matrix
        rows = []
        with patch.object(terminal_matrix, 'DEVICE', object()), patch.object(terminal_matrix, 'PEER_CUT', []), \
                patch.object(terminal_matrix, 'row', lambda *row: rows.append(row)):
            terminal_matrix.control_plane_down(None, [])
        self.assertEqual([(ok, 'SMOKE_PEER_CUT' in why) for _, ok, why in rows], [(False, True)])

    def test_the_sudo_password_reaches_sudo_only_and_the_script_arrives_whole(self):
        for needs_password in (True, False):
            with self.subTest(needs_password=needs_password), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                # ssh runs the remote command here; sudo records what it was given instead of running the script
                (root / 'ssh').write_text('#!/bin/sh\nfor last; do :; done\nexec bash -c "$last"\n')
                (root / 'sudo').write_text(
                    '#!/bin/bash\n'
                    'if [ "$1 $2" = "-n true" ]; then exit %d; fi\n'
                    'if [ "$1" = -S ]; then IFS= read -r given; printf %%s "$given" >"$OUT/password"; fi\n'
                    'printf "%%s\\n" "$@" >"$OUT/args"; cat >"$OUT/stdin"\n' % (1 if needs_password else 0))
                for name in ('ssh', 'sudo'):
                    (root / name).chmod(0o755)
                (root / 'secret').write_text("p w$'\"d")  # no final newline, shell characters
                env = dict(os.environ, PATH=directory + os.pathsep + os.environ['PATH'], OUT=directory,
                           PB_CUT_SUDO_FILE=str(root / 'secret'))
                done = subprocess.run([str(HERE / 'host_cut.sh'), 'h', '1000', 'add', 'server', '1.2.3.4'],
                                      capture_output=True, text=True, env=env, timeout=30)
                self.assertEqual(done.returncode, 0, done.stderr)
                args = (root / 'args').read_text()
                self.assertIn('ready() {', args)
                self.assertIn('systemd-run', args)
                self.assertTrue(args.rstrip().endswith('host_cut\n1000\nadd\nserver\n1.2.3.4'), args[-200:])
                self.assertNotIn("p w$'", args + done.stdout + done.stderr)
                self.assertEqual((root / 'stdin').read_text(), '')
                if needs_password:
                    self.assertEqual((root / 'password').read_text(), "p w$'\"d")
                else:
                    self.assertFalse((root / 'password').exists())

    def test_the_host_cut_refuses_a_bad_request_before_it_connects(self):
        with tempfile.TemporaryDirectory() as directory:
            ssh = Path(directory) / 'ssh'
            ssh.write_text('#!/bin/sh\necho CONNECTED\n')
            ssh.chmod(0o755)
            env = dict(os.environ, PATH=directory + os.pathsep + os.environ['PATH'])
            for arguments in (['h', 'root', 'check'], ['h', '1000', 'add', 'silent', '1.2.3.4'], ['h', '1000', 'add', 'server'],
                              ['h', '1000', 'add', 'server', '1.2.3.4;reboot'], ['h', '1000', 'add', 'all', '1.2.3.4'],
                              ['h', '1000', 'remove', 'x']):
                done = subprocess.run([str(HERE / 'host_cut.sh'), *arguments], capture_output=True, text=True, env=env, timeout=30)
                self.assertNotEqual(done.returncode, 0, arguments)
                self.assertNotIn('CONNECTED', done.stdout, arguments)
            done = subprocess.run([str(HERE / 'host_cut.sh'), 'h', '1000', 'add', 'server', '1.2.3.4', 'fd00::1'],
                                  capture_output=True, text=True, env=env, timeout=30)
            self.assertIn('CONNECTED', done.stdout)


if __name__ == '__main__':
    unittest.main()
