import json
import os
import pathlib
import shlex
import stat
import subprocess
import tempfile
import unittest
from unittest import mock

import ssh_mac_rows
import windows_checks

HERE = pathlib.Path(__file__).resolve().parent
TOKEN = 'Zq9TOKENTOKENTOKENTOKENabcdef0123456789'
# ssh stub: logs every call; `bash -s` (the remote build script) prints a pairing result, or fails with a token in its output.
SSH = '''#!/bin/bash
echo "ssh $*" >> "$STUB_LOG"
cat > /dev/null 2>&1 || true
case "$*" in
  *"bash -s"*) if [ -n "${STUB_FAIL:-}" ]; then echo "boom PB_TOKEN='%s'"; exit 1; fi; echo MACHINE_ID=machine_0a1b-2c3d ;;
esac
exit 0
''' % TOKEN
CLEANUP = '#!/bin/bash\necho "cleanup $*" >> "$STUB_LOG"\n[ -z "${STUB_CLEANUP_FAIL:-}" ]\n'


class Script(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        d = pathlib.Path(self.tmp.name)
        for name, body in (('ssh', SSH), ('cleanup', CLEANUP)):
            (d / name).write_text(body)
            (d / name).chmod(0o755)
        (d / 'pb').write_bytes(b'build')
        self.log = d / 'log'
        self.d = d

    def tearDown(self):
        self.tmp.cleanup()

    def sh(self, *args, **env):
        e = dict(os.environ, PATH='%s:%s' % (self.d, os.environ['PATH']), STUB_LOG=str(self.log),
                 REVOKE_CLIENT_CLEANUP=str(self.d / 'cleanup'), **env)
        return subprocess.run([str(HERE / 'revoke_client.sh')] + list(args), capture_output=True, text=True, env=e, timeout=60, stdin=subprocess.DEVNULL)

    def calls(self):
        return self.log.read_text() if self.log.exists() else ''

    def test_up_prints_one_json_line_with_the_command(self):
        p = self.sh('up', 'hostx', 'test-rc-1', str(self.d / 'pb'))
        self.assertEqual(0, p.returncode, p.stderr)
        info = json.loads(p.stdout)
        self.assertEqual(['command', 'machine_id', 'name'], sorted(info))
        self.assertEqual(('test-rc-1', 'machine_0a1b-2c3d'), (info['name'], info['machine_id']))
        argv = shlex.split(info['command'])
        self.assertEqual(['ssh', 'hostx', 'sudo', '-n', 'docker', 'exec'], argv[:6])
        self.assertEqual('test-rc-1', argv[-1])
        self.assertNotIn(TOKEN, p.stdout + p.stderr)

    def test_failed_up_removes_what_it_made_and_leaks_no_token(self):
        p = self.sh('up', 'hostx', 'test-rc-2', str(self.d / 'pb'), STUB_FAIL='1')
        self.assertEqual(1, p.returncode)
        self.assertEqual('', p.stdout)
        self.assertIn('pairing the throwaway client on hostx failed', p.stderr)
        self.assertNotIn(TOKEN, p.stdout + p.stderr)
        self.assertIn('docker rm -f test-rc-2', self.calls())

    def test_down_removes_the_container_and_revokes_by_exact_id(self):
        p = self.sh('down', 'hostx', 'test-rc-3', 'machine_0a1b-2c3d')
        self.assertEqual(0, p.returncode, p.stderr)
        self.assertIn('docker rm -f test-rc-3', self.calls())
        self.assertIn('cleanup --id machine_0a1b-2c3d --stale 0 seconds --apply', self.calls())

    def test_down_without_an_id_only_removes_the_container(self):
        self.assertEqual(0, self.sh('down', 'hostx', 'test-rc-3').returncode)
        self.assertNotIn('cleanup', self.calls())

    def test_a_failed_revoke_is_reported(self):
        p = self.sh('down', 'hostx', 'test-rc-3', 'machine_1', STUB_CLEANUP_FAIL='1')
        self.assertEqual(1, p.returncode)
        self.assertIn('revoke of machine_1 failed', p.stderr)

    def test_bad_arguments_are_refused(self):
        self.assertEqual(2, self.sh('up', 'hostx', 'bad name;x', 'b').returncode)


class Wiring(unittest.TestCase):
    def setUp(self):
        ssh_mac_rows.ARGS = mock.Mock(alias='macmini', mac_user='admin')
        ssh_mac_rows.RESULTS.clear()
        self.calls = []

    def fake_sh(self, up_rc=0):
        def sh(*args):
            self.calls.append(args)
            if args[0] == 'up':
                return up_rc, ('' if up_rc else json.dumps({'name': args[2], 'machine_id': 'machine_9', 'command': 'ssh h docker exec c'}) + '\n'), \
                    ('no docker' if up_rc else '')
            return 0, '', ''
        return sh

    def outage(self, env, sh=None):
        answer = lambda s, timeout=300: 'sudo-ok warm-ok'
        with mock.patch.dict('os.environ', env, clear=True), mock.patch.object(ssh_mac_rows, 'revoke_client_sh', sh or self.fake_sh()), \
                mock.patch.object(ssh_mac_rows, 'cl', answer), mock.patch.object(ssh_mac_rows, 'rcl', answer), \
                mock.patch.object(ssh_mac_rows, 'mac', lambda s, timeout=120: ''), mock.patch.object(ssh_mac_rows, 'msudo', lambda s, **k: ''):
            ssh_mac_rows.outage()
        return ssh_mac_rows.RESULTS

    def test_unset_and_unconfigured_still_skips(self):
        self.assertEqual('SKIP', self.outage({})[0]['status'])
        self.assertEqual([], self.calls)

    def test_unset_but_configured_runs_the_step_with_a_made_client_and_removes_it(self):
        res = self.outage({'REVOKE_CLIENT_HOST': 'h', 'REVOKE_CLIENT_BIN': '/b'})
        self.assertNotEqual('SKIP', res[0]['status'])  # the step ran (it fails on the stubbed Mac: sshd file)
        self.assertIn('authorized_keys', res[0]['detail'])
        self.assertEqual(['up', 'down'], [c[0] for c in self.calls])
        self.assertEqual('machine_9', self.calls[1][3])
        self.assertNotIn('REVOKE_CLIENT_CMD', os.environ)

    def test_the_step_sees_the_made_command(self):
        seen = []
        with mock.patch.dict('os.environ', {'REVOKE_CLIENT_HOST': 'h', 'REVOKE_CLIENT_BIN': '/b'}, clear=True), \
                mock.patch.object(ssh_mac_rows, 'revoke_client_sh', self.fake_sh()):
            ssh_mac_rows.own_revoke_client('F5-10 revoke')(lambda: seen.append(os.environ['REVOKE_CLIENT_CMD']))()
        self.assertEqual(['ssh h docker exec c'], seen)

    def test_a_failed_creation_is_a_loud_fail_and_still_cleans_up(self):
        res = self.outage({'REVOKE_CLIENT_HOST': 'h', 'REVOKE_CLIENT_BIN': '/b'}, sh=self.fake_sh(up_rc=1))
        self.assertEqual('FAIL', res[0]['status'])
        self.assertIn('no docker', res[0]['detail'])
        self.assertEqual(['up', 'down'], [c[0] for c in self.calls])

    def test_teardown_runs_when_the_step_raises(self):
        def boom():
            raise RuntimeError('step crashed')
        with mock.patch.dict('os.environ', {'REVOKE_CLIENT_HOST': 'h', 'REVOKE_CLIENT_BIN': '/b'}, clear=True), \
                mock.patch.object(ssh_mac_rows, 'revoke_client_sh', self.fake_sh()):
            with self.assertRaises(RuntimeError):
                ssh_mac_rows.own_revoke_client('F5-10 revoke')(boom)()
            self.assertNotIn('REVOKE_CLIENT_CMD', os.environ)
        self.assertEqual(['up', 'down'], [c[0] for c in self.calls])

    def test_a_host_without_a_build_fails_instead_of_skipping(self):
        res = self.outage({'REVOKE_CLIENT_HOST': 'h'})
        self.assertEqual('FAIL', res[0]['status'])
        self.assertEqual([], self.calls)

    def test_a_given_command_is_left_alone(self):
        res = self.outage({'REVOKE_CLIENT_CMD': 'x', 'REVOKE_CLIENT_HOST': 'h', 'REVOKE_CLIENT_BIN': '/b'})
        self.assertEqual([], self.calls)

    def test_windows_checks_does_not_skip_when_a_host_is_configured(self):
        with mock.patch.dict('os.environ', {'REVOKE_CLIENT_HOST': 'h'}, clear=True):
            self.assertEqual([], windows_checks.missing(['REVOKE_CLIENT_CMD'], mock.Mock()))
        with mock.patch.dict('os.environ', {}, clear=True):
            self.assertEqual(['REVOKE_CLIENT_CMD'], windows_checks.missing(['REVOKE_CLIENT_CMD'], mock.Mock()))


if __name__ == '__main__':
    unittest.main()
