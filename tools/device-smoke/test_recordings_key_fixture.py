import json
import os
import pathlib
import subprocess
import tempfile
import unittest

HERE = pathlib.Path(__file__).resolve().parent
TOKEN = 'Zq9TOKENTOKENTOKENTOKENabcdef0123456789'
# ssh stub: logs every call and the script it is fed. `bash -s` of `up` answers like the product; `down` scripts are only logged.
SSH = '''#!/bin/bash
echo "ssh $*" >> "$STUB_LOG"
script=$(cat)
echo "$script" >> "$STUB_LOG"
case "$*" in
  *"ct="*"bash -s"*) if [ -n "${STUB_FAIL:-}" ]; then echo "boom PB_TOKEN='%s'"; exit 1; fi
     echo MACHINE_ID=machine_0a1b-2c3d; [ -n "${STUB_HIDDEN:-}" ] || echo TEAM_VISIBLE=1 ;;
  *"id="*"bash -s"*) [ -z "${STUB_REVOKE_FAIL:-}" ] || exit 1 ;;
esac
exit 0
''' % TOKEN


class Fixture(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.d = pathlib.Path(self.tmp.name)
        (self.d / 'ssh').write_text(SSH)
        (self.d / 'ssh').chmod(0o755)
        self.log = self.d / 'log'

    def tearDown(self):
        self.tmp.cleanup()

    def sh(self, *args, **env):
        e = dict(os.environ, PATH='%s:%s' % (self.d, os.environ['PATH']), STUB_LOG=str(self.log), **env)
        return subprocess.run([str(HERE / 'recordings_key_fixture.sh')] + list(args), capture_output=True, text=True, env=e, timeout=60, stdin=subprocess.DEVNULL)

    def calls(self):
        return self.log.read_text() if self.log.exists() else ''

    def test_up_transfers_the_machine_to_the_team_and_prints_its_id(self):
        p = self.sh('up', 'hostx', 'sh-r1-1', 'team_1')
        self.assertEqual(0, p.returncode, p.stderr)
        self.assertEqual({'name': 'sh-r1-1', 'machine_id': 'machine_0a1b-2c3d'}, json.loads(p.stdout))
        self.assertIn('transfer-to-team', self.calls())  # sharing alone leaves the machine invisible to `pb session list --workspace`
        self.assertNotIn(TOKEN, p.stdout + p.stderr)

    def test_a_machine_that_is_not_team_visible_fails_and_is_revoked_by_exact_id(self):
        p = self.sh('up', 'hostx', 'sh-r1-1', 'team_1', STUB_HIDDEN='1')
        self.assertEqual(1, p.returncode)
        self.assertIn('did not become a team machine', p.stderr)
        self.assertIn('id=machine_0a1b-2c3d', self.calls())
        self.assertIn('docker rm -f sh-r1-1', self.calls())

    def test_a_failed_up_removes_the_container_and_redacts_the_token(self):
        p = self.sh('up', 'hostx', 'sh-r1-1', 'team_1', STUB_FAIL='1')
        self.assertEqual(1, p.returncode)
        self.assertNotIn(TOKEN, p.stdout + p.stderr)
        self.assertIn('docker rm -f sh-r1-1', self.calls())

    def test_down_removes_from_team_revokes_in_two_steps_and_cleans_files(self):
        p = self.sh('down', 'hostx', 'sh-r1-1', 'team_1', 'machine_0a1b-2c3d')
        self.assertEqual(0, p.returncode, p.stderr)
        log = self.calls()
        for part in ('team machine remove', 'machine revoke "$id" --json', '--confirm "$token"', 'docker rm -f sh-r1-1', 'rm -rf /tmp/sh-r1-1-pb /tmp/sh-r1-1-files'):
            self.assertIn(part, log)
        self.assertLess(log.index('team machine remove'), log.index('--confirm "$token"'))

    def test_down_reports_a_failed_revoke_but_still_removes_the_container(self):
        p = self.sh('down', 'hostx', 'sh-r1-1', 'team_1', 'machine_0a1b-2c3d', STUB_REVOKE_FAIL='1')
        self.assertEqual(1, p.returncode)
        self.assertIn('revoke of machine_0a1b-2c3d failed', p.stderr)
        self.assertIn('docker rm -f sh-r1-1', self.calls())

    def test_down_without_a_machine_id_only_removes_the_container(self):
        p = self.sh('down', 'hostx', 'sh-r1-1', 'team_1')
        self.assertEqual(0, p.returncode)
        self.assertNotIn('revoke', self.calls())

    def test_bad_names_are_refused(self):
        self.assertEqual(2, self.sh('up', 'hostx', 'bad name', 'team_1').returncode)
        self.assertEqual(2, self.sh('up', 'hostx', 'sh-r1-1', 'team 1').returncode)


class SmokeRunner(unittest.TestCase):
    """The test_recording_key_rotation function of smoke.sh, run alone with record/skipped/now stubbed."""

    def run_function(self, env, fixture_out='', fixture_rc=0, rotation_out='', rotation_rc=0):
        text = (HERE / 'smoke.sh').read_text()
        start = text.index('test_recording_key_rotation() {')
        function = text[start:text.index('\n}\n', start) + 3]
        with tempfile.TemporaryDirectory() as tmp:
            d = pathlib.Path(tmp)
            (d / 'recordings_key_fixture.sh').write_text('#!/bin/bash\necho "$@" >> %s/fixture.log\n[ "$1" = up ] || exit 0\n'
                                                         'echo %r; exit %d\n' % (d, fixture_out, fixture_rc))
            (d / 'recordings_key_fixture.sh').chmod(0o755)
            (d / 'one.py').write_text('')
            (d / 'ssh').write_text('#!/bin/bash\ncat > /dev/null\ncase "$*" in *recordings_key_rotation.py*) echo %r; exit %d;; esac\n' % (rotation_out, rotation_rc))
            (d / 'ssh').chmod(0o755)
            script = ('HERE=%s; SSH_OPTS=(); skipped() { return 1; }; now() { echo 0; }; '
                      'record() { echo "$1|$2|$4"; }\n%s\ntest_recording_key_rotation\n') % (d, function)
            result = subprocess.run(['bash', '-c', script], capture_output=True, text=True, timeout=60, stdin=subprocess.DEVNULL,
                                    env=dict(os.environ, PATH='%s:%s' % (d, os.environ['PATH']), **env))
            log = (d / 'fixture.log').read_text() if (d / 'fixture.log').exists() else ''
        return result.stdout.strip(), log

    ENV = {'SMOKE_RECKEY_HOST': 'hostx', 'SMOKE_RECKEY_TEAM': 'team_1', 'SMOKE_RECKEY_VAULT_PASSWORD_FILE': '/home/x/pw'}

    def test_a_missing_environment_is_a_visible_skip_naming_it(self):
        out, log = self.run_function({'SMOKE_RECKEY_HOST': 'hostx', 'SMOKE_RECKEY_TEAM': '', 'SMOKE_RECKEY_VAULT_PASSWORD_FILE': '', 'SMOKE_LINUX_SSH': ''})
        self.assertEqual('recording-key-rotation|SKIP|needs SMOKE_RECKEY_TEAM SMOKE_RECKEY_VAULT_PASSWORD_FILE', out)
        self.assertEqual('', log)  # nothing was built

    def test_a_passing_receipt_is_a_pass_and_the_fixture_is_always_torn_down(self):
        receipt = json.dumps({'check': 'recording-key-rotation', 'status': 'PASS', 'steps': [{'step': 'replace', 'status': 'PASS'}],
                              'generations': {'replace': 2, 'reset': 3}})
        out, log = self.run_function(self.ENV, '{"name": "n", "machine_id": "machine_1"}', 0, receipt, 0)
        self.assertEqual('recording-key-rotation|PASS', out[:27], out)
        self.assertIn('replace->generation 2, reset->3', out)
        self.assertIn('machine_1', log.splitlines()[-1])  # down names the exact id

    def test_a_failing_receipt_is_a_fail_with_the_first_failed_steps_and_still_tears_down(self):
        receipt = json.dumps({'check': 'recording-key-rotation', 'status': 'FAIL', 'generations': {},
                              'steps': [{'step': 'replace', 'status': 'FAIL', 'error': 'pb recordings key replace failed: exit 1'}]})
        out, log = self.run_function(self.ENV, '{"name": "n", "machine_id": "machine_1"}', 0, receipt, 1)
        self.assertTrue(out.startswith('recording-key-rotation|FAIL|'), out)
        self.assertIn('replace: pb recordings key replace failed: exit 1', out)
        self.assertTrue(log.splitlines()[-1].startswith('down'))

    def test_a_failed_fixture_is_a_fail_and_runs_down(self):
        out, log = self.run_function(self.ENV, 'recordings_key_fixture: pairing failed', 1)
        self.assertIn('recording-key-rotation|FAIL|fixture: recordings_key_fixture: pairing failed', out)
        self.assertEqual(['up', 'down'], [line.split()[0] for line in log.splitlines()])


# A host-in-a-directory: ssh runs the remote command locally, with fake sudo/docker (logged) and a fake owner pb.
EXEC_SSH = '#!/bin/bash\nshift\nbash -c "$*"\n'
FAKE_DOCKER = '''#!/bin/bash
echo "docker $*" >> "$STUB_LOG"
case "$*" in *" pair "*) echo '{"ok": true, "data": {"machine_id": "machine_0a1b-2c3d"}}' ;; esac
case "$*" in *" -i "*) cat > /dev/null ;; esac
'''
FAKE_SUDO = '#!/bin/bash\nshift; [ "$1" = -n ] && shift\nexec "$@"\n'  # sudo [-n] docker ...
FAKE_PB = '''#!/bin/bash
echo "pb $*" >> "$STUB_LOG"
case "$*" in
  "machine add"*) echo '{"data": {"linux_macos_command": "PB_TOKEN='"'"'abc123'"'"'"}}' ;;
  "team get"*) echo '{"data": {"generation": 4, "machines": [{"machine_id": "machine_0a1b-2c3d", "active": true}]}}' ;;
  "machine revoke"*" --confirm "*) exit 0 ;;
  "machine revoke"*) echo '{"data": {"confirmation_token": "tok"}}' ;;
esac
'''


class Executed(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.d = pathlib.Path(self.tmp.name)
        self.log = self.d / 'log'
        (self.d / 'home/.local/bin').mkdir(parents=True)
        for path, body in (('ssh', EXEC_SSH), ('sudo', FAKE_SUDO), ('docker', FAKE_DOCKER), ('sleep', '#!/bin/bash\n'), ('home/.local/bin/pb', FAKE_PB)):
            (self.d / path).write_text(body)
            (self.d / path).chmod(0o755)

    def tearDown(self):
        self.tmp.cleanup()

    def sh(self, *args, **env):
        e = dict(os.environ, PATH='%s:%s' % (self.d, os.environ['PATH']), HOME=str(self.d / 'home'), STUB_LOG=str(self.log), **env)
        return subprocess.run([str(HERE / 'recordings_key_fixture.sh')] + list(args), capture_output=True, text=True, env=e, timeout=60, stdin=subprocess.DEVNULL)

    def docker_lines(self):
        return [l for l in self.log.read_text().splitlines() if l.startswith('docker ')]

    def test_defaults_are_the_harness_defaults(self):
        p = self.sh('up', 'hostx', 'sh-r1-d', 'team_1')
        self.assertEqual(0, p.returncode, p.stderr)
        lines = '\n'.join(self.docker_lines())
        self.assertIn('--tmpfs /run/lock test-systemd', lines)
        self.assertIn('exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 sh-r1-d', lines)
        self.assertIn('systemctl start user@1000.service', lines)

    def test_the_harness_container_settings_pick_the_image_and_the_user(self):
        p = self.sh('up', 'hostx', 'sh-r1-d', 'team_1', SMOKE_CT_IMAGE='gap5-systemd', SMOKE_CT_USER='tester', SMOKE_CT_UID='1001', SMOKE_CT_DOCKER='docker')
        self.assertEqual(0, p.returncode, p.stderr)
        lines = '\n'.join(self.docker_lines())
        self.assertIn('--tmpfs /run/lock gap5-systemd', lines)
        self.assertNotIn('test-systemd', lines)
        self.assertIn('exec -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 sh-r1-d', lines)
        self.assertIn('loginctl enable-linger tester', lines)
        self.assertIn('systemctl start user@1001.service', lines)
        self.assertIn('cp /tmp/sh-r1-d-pb sh-r1-d:/home/tester/pb', lines)
        self.assertNotIn('pbt', lines)
        self.assertNotIn('1000', lines)
