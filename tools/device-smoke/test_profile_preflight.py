import contextlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import profile_preflight as p
import windows_checks

HERE = Path(__file__).resolve().parent
# A pb that records every call and answers `team list` only for the profiles named in $SIGNED_IN. Its `auth status`
# says signed in whatever the server thinks, as the real one does for an expired session.
FAKE_PB = '''#!/bin/sh
echo "$*" >>"$CALLS"
profile=default
[ "$1" != --config ] || { profile=teammate; shift 2; }
if [ "$1 $2" = "auth status" ]; then echo '{"signed_in":true}'; exit 0; fi
case " $SIGNED_IN " in *" $profile "*) echo '[]' ;; *)
  echo '{"schema_version":"1.0","ok":false,"error":{"code":"session_expired","message":"secret-looking text"}}'; exit 3 ;; esac
'''


class Probe(unittest.TestCase):
    def home(self, signed_in, teammate_profile=True):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        home = Path(directory.name)
        pb = home / '.local/bin/pb'
        pb.parent.mkdir(parents=True)
        pb.write_text(FAKE_PB)
        pb.chmod(0o700)
        calls = home / 'calls'
        calls.touch()
        patch = mock.patch.dict(os.environ, HOME=str(home), CALLS=str(calls), SIGNED_IN=signed_in)
        patch.start()
        self.addCleanup(patch.stop)
        os.environ.pop('PB_SMOKE_WINDOWS_PB', None)
        if teammate_profile:
            root = p.profile_root()
            root.mkdir(parents=True)
            (root / 'config.json').write_text('{}')
        return calls

    def test_each_profile_is_proven_by_a_call_the_server_answers_and_nothing_signs_in(self):
        calls = self.home('default')
        self.assertIsNone(p.probe(p.DEFAULT))
        self.assertEqual('`pb team list` failed: session_expired', p.probe(p.TEAMMATE))
        self.assertEqual(['team list --json', '--config %s team list --json' % (p.profile_root() / 'config.json')],
                         calls.read_text().splitlines())

    def test_a_profile_that_was_never_set_up_is_reported_without_running_pb(self):
        calls = self.home('default teammate', teammate_profile=False)
        self.assertIn('never signed in', p.probe(p.TEAMMATE))
        self.assertEqual('', calls.read_text())

    def test_a_probe_that_cannot_run_is_a_reason_not_a_traceback(self):
        def broken():
            raise OSError('no such device')
        self.assertEqual('the probe could not run: OSError', p.problem(broken, sleep=lambda seconds: None))
        answers = iter(['`pb team list` failed: exit 1', None])
        self.assertIsNone(p.problem(lambda: next(answers), sleep=lambda seconds: None))
        self.assertEqual('`pb team list` failed: exit 7', p.answer(7, 'not json: token abc'))

    def test_the_device_form_names_the_profile_and_the_command_that_signs_it_in(self):
        self.home('default')
        output = io.StringIO()
        with contextlib.redirect_stdout(output), mock.patch.object(p.time, 'sleep'):
            self.assertEqual(0, p.main(['--machine', 'box', '--container', 'dockerhost:box', 'default', 'teammate']))
        default, teammate = output.getvalue().splitlines()
        self.assertEqual('PROFILE default OK', default)
        self.assertIn('second (teammate) profile on box is signed out (`pb team list` failed: session_expired)', teammate)
        self.assertIn('python3 tools/device-smoke/teammate_signin.py dockerhost box pbt', teammate)
        self.assertNotIn('secret-looking', teammate)

    def test_sign_in_commands(self):
        self.assertIn('teammate_signin.py --windows-install-json /r/w.json',
                      p.signin_command(p.TEAMMATE, 'win', r'C:\pb.exe', '/r/w.json'))
        self.assertIn(r"& 'C:\pb.exe' login", p.signin_command(p.DEFAULT, 'win', r'C:\pb.exe'))
        self.assertIn('teammate_login.py', p.signin_command(p.TEAMMATE, 'realhost'))
        self.assertEqual('on realhost: pb login', p.signin_command(p.DEFAULT, 'realhost'))


class WindowsRunner(unittest.TestCase):
    def table(self, **values):
        arguments = ['--install-json', 'unused.json', '--machine', 'win', '--peer', 'peer', '--team', 'team']
        for name, value in values.items():
            arguments += ['--' + name.replace('_', '-'), value]
        args = windows_checks.build_parser().parse_args(arguments)
        return args, windows_checks.checks(args)

    def test_every_check_that_uses_the_second_profile_is_known_to(self):
        args, table = self.table()
        second = {check for check, (argv, _) in table.items()
                  if p.TEAMMATE in windows_checks.profiles_needed(check, argv, args)}
        self.assertEqual({'team-business', 'env-team-scopes', 'team-two-account', 'team-suspension',
                          'env-vault-lifecycle', 'team-lifecycle', 'team-roles', 'team-machines'}, second)
        # A new script that uses the second profile must be added: these import it, or are told to use it.
        for check, (argv, _) in table.items():
            script = (HERE / argv[0]).read_text()
            if 'from teammate_profile import' in script and 'add_argument(\'--teammate\'' not in script:
                self.assertIn(check, second, argv[0])
        self.assertEqual([], windows_checks.profiles_needed('account-login', ['account_login.py'], args))
        self.assertEqual([p.DEFAULT], windows_checks.profiles_needed('exec-main', table['exec-main'][0], args))
        self.assertEqual([p.TEAMMATE],
                         windows_checks.profiles_needed('env-vault-lifecycle', table['env-vault-lifecycle'][0], args))

    def test_a_side_of_the_device_is_a_device_profile_not_a_unix_fixture(self):
        args, table = self.table(member_cmd='windows-teammate', owner_cmd='windows')
        self.assertEqual([p.DEFAULT], windows_checks.profiles_needed('policy-team', table['policy-team'][0], args))
        self.assertEqual([p.DEFAULT, p.TEAMMATE],
                         windows_checks.profiles_needed('team-two-account', table['team-two-account'][0], args))

    def test_unix_fixtures_of_the_runner_are_checked_too(self):
        args, table = self.table(member_cmd='ssh member', owner_cmd='ssh owner')
        self.assertEqual([p.DEFAULT, '--owner-cmd'], windows_checks.profiles_needed('policy-team', table['policy-team'][0], args))
        self.assertEqual([p.DEFAULT, '--member-cmd'],
                         windows_checks.profiles_needed('team-two-account', table['team-two-account'][0], args))
        ran = []

        def run(command, **kwargs):
            ran.append(command)
            return subprocess.CompletedProcess(command, 3, '{"error":{"code":"auth_required"}}', '')
        import policy_team
        with mock.patch.object(windows_checks.subprocess, 'run', run), mock.patch.object(policy_team, 'timeout_words', lambda prefix: []):
            for option in ('--owner-cmd', '--member-cmd'):
                self.assertEqual('`pb team list` failed: auth_required', windows_checks.fixture_probe(option, args))
        self.assertEqual(['ssh', 'owner'], ran[0][:2])
        self.assertTrue(ran[0][-1].endswith('team list --json'), ran[0])
        self.assertEqual(['ssh', 'member', 'bash', '-lc', "'~/.local/bin/pb team list --json'"], ran[1])

    def run_main(self, only, signed_out, run_check):
        with tempfile.TemporaryDirectory() as directory:
            receipt = Path(directory) / 'install.json'
            receipt.write_text('{}')
            device = mock.Mock()
            events = []

            def probe(profile):
                events.append('probe ' + profile)
                return 'session_expired' if profile in signed_out else None

            def recorded(check, *arguments):
                events.append('run ' + check)
                return run_check(check)
            output = io.StringIO()
            with mock.patch.dict(os.environ, SMOKE_RESULTS_DIR=directory), \
                    mock.patch.object(sys, 'argv', ['windows_checks.py', '--install-json', str(receipt), '--machine', 'win',
                                                    '--peer', 'peer', '--team', 'team', '--only', only]), \
                    mock.patch.object(windows_checks.preflight_fixtures, 'main', lambda argv: 0), \
                    mock.patch.object(windows_checks.install_path, 'windows_executable', lambda data: 'C:\\pb\\pb.exe'), \
                    mock.patch.object(windows_checks.winhost, 'device', lambda: device), \
                    mock.patch.object(windows_checks.winhost, 'RemotePath', mock.Mock()), \
                    mock.patch.object(p, 'probe', probe), mock.patch.object(p.time, 'sleep'), \
                    mock.patch.object(windows_checks, 'run_check', recorded), \
                    contextlib.redirect_stdout(output), contextlib.redirect_stderr(io.StringIO()):
                code = windows_checks.main()
            [result] = Path(directory).glob('windows-*.json')
            rows = {row['id']: row for row in json.loads(result.read_text())['results']}
        return code, rows, events

    def test_a_signed_out_second_profile_fails_its_checks_before_any_check_and_the_rest_run(self):
        code, rows, events = self.run_main('exec-main,team-business', {p.TEAMMATE}, lambda check: ('PASS', 1, 'ok'))
        self.assertEqual(1, code)
        self.assertEqual({'exec-main:win': 'PASS', 'team-business:win': 'FAIL'},
                         {key: row['status'] for key, row in rows.items()})
        self.assertIn('second (teammate) profile on win is signed out', rows['team-business:win']['detail'])
        self.assertIn('teammate_signin.py --windows-install-json', rows['team-business:win']['detail'])
        self.assertEqual(['probe default', 'probe teammate', 'probe teammate', 'run exec-main'], events)

    def test_signed_in_profiles_let_every_check_run_and_a_crashing_check_still_has_its_row(self):
        def crash(check):
            raise RuntimeError('the device\'s second profile went away')
        code, rows, events = self.run_main('team-business', set(), crash)
        self.assertEqual((1, 'FAIL'), (code, rows['team-business:win']['status']))
        self.assertIn('RuntimeError: the device', rows['team-business:win']['detail'])
        self.assertEqual(['probe default', 'probe teammate', 'run team-business'], events)


class SmokeShell(unittest.TestCase):
    """smoke.sh's own preflight functions, with the device transport replaced."""

    def run_smoke(self, body, *arguments, targets='alpha,beta'):
        prefix = (HERE / 'smoke.sh').read_text().split('# ---------------------------------------------------------------- run', 1)[0]
        with tempfile.TemporaryDirectory() as directory:
            script = Path(directory) / 'smoke.sh'
            for name in ('snapshot_run.py', 'dev_targets.py', 'stage_fixtures.py', 'last_green.py', 'failure_triage.py',
                         'preview_receipt.py', 'host_sessions.py'):
                shutil.copyfile(HERE / name, Path(directory) / name)
            script.write_text(prefix + r'''
IFS=, read -r -a TLIST <<<"$TARGETS"
TEAMMATE=mate
on() { return 0; }
stage_terminal_fixtures() { return 0; }
device_py() { # DEVICE SCRIPT "flags... PROFILE"
  local device=$1 profile=${3##* }
  case " $SIGNED_OUT " in
    *" $device/$profile "*) echo "PROFILE $profile sign-in preflight: the $profile profile on $device is signed out; sign it in: fix-$device" ;;
    *" $device/unreachable "*) return 255 ;;
    *) echo "PROFILE $profile OK"; : ;;
  esac
}
test_device_application() { local group=$1; shift 3; local device; for device; do skipped "$group:$device" || record "$group:$device" PASS 0 ran; done; }
''' + body + '\nprintf "%s\\n" "${ROWS[@]}"\n')
            environment = dict(os.environ, PB_DEV_TARGETS_FILE=str(HERE / 'targets.example.json'))
            done = subprocess.run(['bash', str(script), '--expect-version', 'fixture', '--targets', targets, *arguments],
                                  env=environment, capture_output=True, text=True, timeout=30)
        self.assertEqual(0, done.returncode, done.stdout + done.stderr)
        return {line.split('|')[0]: line.split('|') for line in done.stdout.splitlines() if line.count('|') >= 3}

    def test_signed_out_profiles_fail_the_rows_that_use_them_and_only_those_do_not_run(self):
        rows = self.run_smoke('SIGNED_OUT="beta/teammate"\nsignin_preflight\ntest_team_checks')
        for group in ('team-lifecycle', 'team-roles'):
            self.assertEqual('FAIL', rows[group + ':beta'][1])
            self.assertIn('sign it in: fix-beta', rows[group + ':beta'][3])
            self.assertEqual(['PASS', 'PASS'], [rows[group + ':alpha'][1], rows[group + ':mate'][1]])
        self.assertEqual(['team-lifecycle:beta', 'team-roles:beta'], sorted(k for k, v in rows.items() if v[1] == 'FAIL'))

    def test_a_signed_out_teammate_device_fails_inbox_approval_and_a_default_sign_in_has_its_own_row(self):
        rows = self.run_smoke('SIGNED_OUT="mate/default alpha/default"\nsignin_preflight')
        for row in ('inbox-approval:alpha', 'inbox-approval:beta', 'sign-in:alpha', 'team-lifecycle:mate'):
            self.assertEqual('FAIL', rows[row][1], row)
        self.assertNotIn('sign-in:beta', rows)

    def test_a_device_that_cannot_be_asked_is_a_fail_row(self):
        rows = self.run_smoke('SIGNED_OUT="beta/unreachable"\nsignin_preflight', '--only-check', 'team-roles:beta')
        self.assertEqual(['team-roles:beta'], [k for k in rows if k.startswith('team-')])
        self.assertIn('could not be checked', rows['team-roles:beta'][3])

    def test_a_selection_without_such_rows_probes_only_the_devices_it_names(self):
        rows = self.run_smoke('SIGNED_OUT="alpha/default beta/default mate/default"\nsignin_preflight',
                              '--only-check', 'exec-main:alpha-alpha')
        self.assertEqual(['sign-in:alpha'], list(rows))

    def test_a_target_name_that_begins_another_is_not_named_by_it(self):
        for selection, named in (('doctor-lifecycle:box-peer', ['sign-in:box-peer']),
                                 ('exec-main:desk-box-peer', ['sign-in:box-peer', 'sign-in:desk'])):
            rows = self.run_smoke('SIGNED_OUT="box/default box-peer/default desk/default"\nsignin_preflight',
                                  '--only-check', selection, targets='box,box-peer,desk')
            self.assertEqual(named, sorted(k for k in rows if k.startswith('sign-in:')), selection)

    def test_the_probe_quotes_every_value_for_the_shell_that_runs_it(self):
        rows = self.run_smoke(r'''WINDOWS_TARGET=alpha WINDOWS_INSTALL_JSON='/tmp/a  b; touch x.json' SIGNED_OUT=
device_py() { local words; eval "words=($3)"; [ "$#" = 3 ] && [ "${words[3]}" = '/tmp/a  b; touch x.json' ] && echo "PROFILE ${words[4]} OK"; }
signin_probe alpha default; record quoted "$([ -z "$SIGNIN_DETAIL" ] && echo PASS || echo FAIL)" 0 "$SIGNIN_DETAIL"''')
        self.assertEqual('PASS', rows['quoted'][1], rows)

    def test_the_preflight_runs_before_the_first_check(self):
        text = (HERE / 'smoke.sh').read_text().split('# ---------------------------------------------------------------- run', 1)[1]
        self.assertLess(text.index('\nsignin_preflight\n'), text.index('check_device "$d"'))


if __name__ == '__main__':
    unittest.main()
