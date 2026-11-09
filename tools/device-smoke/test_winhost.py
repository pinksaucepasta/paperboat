import base64
import json
import os
import pathlib
import subprocess
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import pairing_lifecycle
import winhost

EXE = r'C:\Program Files\Paperboat\bin\pb.exe'


def envelope(rc=0, out=b'', err=b'', timed_out=False):
    return json.dumps({'rc': rc, 'timed_out': timed_out, 'out': base64.b64encode(out).decode(),
                       'err': base64.b64encode(err).decode()}).encode()


class FakeSsh:
    """Stands in for subprocess.run of the ssh command and records what was sent to the device."""

    def __init__(self, *replies):
        self.replies, self.calls = list(replies), []

    def __call__(self, argv, input, capture_output, timeout, env):
        script, payload = input.decode().split('\n')[:2]
        self.calls.append({'argv': argv, 'script': base64.b64decode(script).decode(),
                           'payload': json.loads(base64.b64decode(payload)), 'env': env})
        rc, out = self.replies.pop(0)
        return subprocess.CompletedProcess(argv, rc, out, b'')


class WindowsDeviceTest(unittest.TestCase):
    def device(self, password='secret'):
        return winhost.WindowsDevice(target='user@host', pb_exe=EXE, password=password)

    def test_command_line_is_one_short_encoded_bootstrap_and_carries_no_secret(self):
        argv = self.device().ssh_command()
        self.assertEqual(['sshpass', '-e', 'ssh'], argv[:3])
        self.assertNotIn('secret', ' '.join(argv))
        command = argv[-1]
        self.assertLess(len(command), 1000)
        encoded = command.rsplit(' ', 1)[1]
        self.assertEqual(winhost.BOOTSTRAP, base64.b64decode(encoded).decode('utf-16-le'))
        self.assertEqual(['ssh', '-o', 'BatchMode=yes'], self.device(password=None).ssh_command()[:3])

    def test_run_sends_exact_argv_input_env_and_returns_text_result(self):
        fake = FakeSsh((0, envelope(7, b'out\r\n', b'err')))
        with patch('subprocess.run', fake):
            result = self.device().run([EXE, 'exec', 'my box', '--', 'a"b'], input='in', env={'K': 'v'}, timeout=5)
        sent = fake.calls[0]
        self.assertEqual(winhost.RUN, sent['script'])
        self.assertEqual(EXE, sent['payload']['exe'])
        self.assertEqual('exec "my box" -- a\\"b', sent['payload']['args'])  # CommandLineToArgvW quoting
        self.assertEqual({'K': 'v'}, sent['payload']['env'])
        self.assertEqual('in', base64.b64decode(sent['payload']['input']).decode())
        self.assertEqual(5000, sent['payload']['timeout_ms'])
        self.assertEqual('secret', sent['env']['SSHPASS'])
        self.assertEqual((7, 'out\r\n', 'err'), (result.returncode, result.stdout, result.stderr))

    def test_timeout_is_a_subprocess_timeout_with_the_partial_output(self):
        with patch('subprocess.run', FakeSsh((0, envelope(-1, b'partial', b'', True)))):
            with self.assertRaises(subprocess.TimeoutExpired) as caught:
                self.device().run([EXE, 'x'], timeout=2)
        self.assertEqual(b'partial', caught.exception.output)
        self.assertEqual(2, caught.exception.timeout)

    def test_failures_name_the_step(self):
        with patch('subprocess.run', FakeSsh((1, b''))):
            with self.assertRaisesRegex(winhost.DeviceError, 'run pb.exe failed on the device'):
                self.device().run([EXE, 'x'])
        with patch('subprocess.run', FakeSsh((0, b'not json'))):
            with self.assertRaisesRegex(winhost.DeviceError, 'no result envelope'):
                self.device().run([EXE, 'x'])
        with patch('subprocess.run', side_effect=subprocess.TimeoutExpired('ssh', 3)):
            with self.assertRaisesRegex(winhost.DeviceError, 'hash C:.x: the device did not answer'):
                self.device().sha256(r'C:\x')

    def test_files_round_trip_and_missing_is_file_not_found(self):
        data = bytes(range(256))
        fake = FakeSsh((0, b''), (0, base64.b64encode(data)), (winhost.MISSING, b''), (0, b'ABCDEF\r\n'), (0, b'yes\r\n'))
        with patch('subprocess.run', fake):
            device = self.device()
            path = winhost.RemotePath(device, r'C:\t') / 'a.bin'
            path.write_bytes(data)
            self.assertEqual(data, path.read_bytes())
            with self.assertRaises(FileNotFoundError):
                path.read_bytes()
            self.assertEqual('abcdef'.upper(), path.sha256())
            self.assertTrue(path.exists())
        self.assertEqual(r'C:\t\a.bin', fake.calls[0]['payload']['path'])
        self.assertEqual(data, base64.b64decode(fake.calls[0]['payload']['data']))

    def test_runtime_dir_and_services(self):
        fake = FakeSsh((0, rb'C:\Users\u\AppData\Local' + b'\r\n'), (0, b'{"Name":"PaperboatHostd-u1","Status":"Running","StartType":"Automatic"}'),
                       (0, b''))
        with patch('subprocess.run', fake):
            device = self.device()
            self.assertEqual(r'C:\Users\u\AppData\Local\Paperboat\runtime', str(device.runtime_dir()))
            self.assertEqual('Running', device.services()[0]['Status'])
            self.assertEqual([], device.services('Nothing*'))
        self.assertEqual('STATE', fake.calls[0]['payload']['name'])

    def test_pb_path_and_command_result_route_to_the_device(self):
        with patch.dict(os.environ, {'PB_SMOKE_WINDOWS_PB': EXE, 'VICTUS_PASS': 'p'}):
            pb = winhost.pb_path()
            fake = FakeSsh((0, envelope(0, b'{"ok":true,"data":{"x":1}}')))
            with patch('subprocess.run', fake):
                self.assertEqual({'x': 1}, pairing_lifecycle.invoke(pb, 'auth', 'status', '--json'))
            self.assertEqual('auth status --json', fake.calls[0]['payload']['args'])
            self.assertEqual(EXE, str(pb))
        with patch.dict(os.environ, {}, clear=True):
            self.assertEqual(pathlib.Path.home() / '.local/bin/pb', winhost.pb_path())
            self.assertIsNone(winhost.device())

    def test_pb_exe_must_be_an_absolute_windows_path(self):
        for bad in ('pb.exe', r'C:\x\other.exe', '/usr/bin/pb.exe'):
            with patch.dict(os.environ, {'PB_SMOKE_WINDOWS_PB': bad}):
                with self.assertRaises(ValueError):
                    winhost.device()

    def test_temp_dir_is_removed_even_when_the_check_fails(self):
        fake = FakeSsh((0, rb'C:\Temp'), (0, b''), (0, b''))
        with patch('subprocess.run', fake):
            with self.assertRaises(KeyError):
                with self.device().temp_dir('pb-x') as directory:
                    self.assertTrue(str(directory).startswith('C:\\Temp\\pb-x-'))
                    raise KeyError
        self.assertIn('Remove-Item', fake.calls[-1]['script'])
        self.assertEqual(str(directory), fake.calls[-1]['payload']['path'])



PWSH = os.environ.get('PB_TEST_PWSH') or __import__('shutil').which('pwsh')


class LocalDevice(winhost.WindowsDevice):
    """Runs the real snippets in a local PowerShell 7 instead of over SSH (PS 5.1 on the device is the same API)."""

    path_flavor = pathlib.PurePosixPath

    def ssh_command(self):
        return [PWSH, '-NoProfile', '-NonInteractive', '-Command', winhost.BOOTSTRAP]


@unittest.skipUnless(PWSH, 'set PB_TEST_PWSH to a pwsh binary to run the snippets for real')
class SnippetsInPowerShell(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        import tempfile
        cls.bin = tempfile.TemporaryDirectory()
        taskkill = pathlib.Path(cls.bin.name) / 'taskkill.exe'
        taskkill.write_text('#!/bin/sh\nfor last; do :; done\nkill -9 "$last" 2>/dev/null\nexit 0\n')
        taskkill.chmod(0o755)
        shim = pathlib.Path(cls.bin.name) / 'powershell.exe'
        shim.write_text('#!/bin/sh\nexec "%s" "$@"\n' % PWSH)
        shim.chmod(0o755)
        shim = pathlib.Path(cls.bin.name) / 'powershell.exe'
        shim.write_text('#!/bin/sh\nexec "%s" "$@"\n' % PWSH)
        shim.chmod(0o755)
        fake = pathlib.Path(cls.bin.name) / 'pb.exe'
        fake.write_text('#!/bin/sh\nif [ "$1" = sleep ]; then sleep 30; fi\nprintf "args=%s|env=%s|in=" "$*" "$PB_FIXTURE"\ncat\n'
                        'echo err-line >&2\nexit "${PB_EXIT:-0}"\n')
        fake.chmod(0o755)
        os.environ['PATH'] = cls.bin.name + os.pathsep + os.environ['PATH']
        cls.exe = str(fake)
        cls.device = LocalDevice(pb_exe=cls.exe, password='')
        cls.device._environment = {'TEMP': tempfile.gettempdir()}

    def test_run_is_byte_exact_with_separate_streams_input_env_and_exit_code(self):
        result = self.device.run([self.exe, 'a b', 'c"d'], input='héllo\n', env={'PB_FIXTURE': 'x y', 'PB_EXIT': '7'})
        self.assertEqual(7, result.returncode)
        self.assertEqual('args=a b c"d|env=x y|in=héllo\n', result.stdout)
        self.assertEqual('err-line\n', result.stderr)

    def test_timeout_kills_the_process_and_raises(self):
        with self.assertRaises(subprocess.TimeoutExpired):
            self.device.run([self.exe, 'sleep'], timeout=1)

    def test_files_and_hash(self):
        with self.device.temp_dir('pb-test') as directory:
            path = directory / 'sub' / 'f.bin'
            data = bytes(range(256)) * 3
            path.write_bytes(data)
            self.assertEqual(data, path.read_bytes())
            self.assertEqual(len(data), path.size())
            self.assertEqual(__import__('hashlib').sha256(data).hexdigest(), path.sha256())
            path.unlink()
            self.assertFalse(path.exists())
            with self.assertRaises(FileNotFoundError):
                path.read_bytes()
        self.assertFalse(directory.exists())

    def test_spawn_collects_output_and_exit_code_and_can_be_killed(self):
        with self.device.temp_dir('pb-test') as directory:
            done = self.device.spawn([self.exe, 'x'], directory)
            self.assertEqual(0, done.wait(30, interval=0.5))
            self.assertTrue(done.stdout().startswith(b'args=x|env=|in='))
            self.assertEqual(b'err-line\n', done.stderr())
        with self.device.temp_dir('pb-test') as directory:
            running = self.device.spawn([self.exe, 'sleep'], directory)
            self.assertTrue(self.device.alive(running.pid))
            running.kill()
            self.assertFalse(self.device.alive(running.pid))

    def test_origin_serves_its_body(self):
        import urllib.request
        with self.device.origin('synthetic <origin> "body"') as port:
            for _ in range(3):
                with urllib.request.urlopen('http://127.0.0.1:%d/x' % port, timeout=10) as response:
                    self.assertEqual('synthetic <origin> "body"', response.read().decode())


class WindowsChecksRunner(unittest.TestCase):
    def args(self, **values):
        defaults = dict(machine='m', peer='p', team='t')
        return SimpleNamespace(**dict(defaults, **values))

    def test_verdict_needs_exit_zero_and_only_pass_statuses(self):
        import windows_checks
        passed = 'noise\n{"check":"a","status":"PASS"}\n{"row":"C1","status":"PASS"}\n'
        self.assertEqual('PASS', windows_checks.verdict(0, passed))
        self.assertEqual('FAIL', windows_checks.verdict(1, passed))
        self.assertEqual('FAIL', windows_checks.verdict(0, passed + '{"status":"FAIL"}\n'))
        self.assertEqual('FAIL', windows_checks.verdict(0, 'no json at all\n'))

    def test_every_check_names_a_script_and_missing_fixtures_are_reported(self):
        import windows_checks
        table = windows_checks.checks(self.args())
        for check, (argv, needs) in table.items():
            self.assertTrue((pathlib.Path(windows_checks.HERE) / argv[0]).exists(), check)
        with patch.dict(os.environ, {}, clear=True):
            self.assertEqual(['FINAL_FIXTURES_URL', 'FINAL_FIXTURES_TOKEN_FILE', 'SMOKE_DNS_ZONE'],
                             windows_checks.missing(table['tunnel-domain'][1], self.args()))
            self.assertEqual(['--team'], windows_checks.missing(table['team-commands'][1], self.args(team='')))
            self.assertEqual([], windows_checks.missing(table['account-inventory'][1], self.args()))


class PortedScriptsRouteToTheDevice(unittest.TestCase):
    """The ported checks reach pb.exe and the runtime directory only through winhost when PB_SMOKE_WINDOWS_PB is set."""

    def test_registration_reads_the_device_runtime_directory(self):
        import install_lifecycle
        record = {k: k + '-value' for k in ('machine_id', 'environment_id', 'public_identity_key', 'public_key_id',
                                            'installation_generation', 'account_id', 'inbox_path')}
        fake = FakeSsh((0, rb'C:\Users\u\AppData\Local'), (0, base64.b64encode(json.dumps(record).encode())))
        with patch.dict(os.environ, {'PB_SMOKE_WINDOWS_PB': r'C:\x\pb.exe', 'VICTUS_PASS': 'p'}), \
                patch('subprocess.run', fake):
            winhost._device.cache_clear()
            self.assertEqual(record, install_lifecycle.registration())
        self.assertEqual(r'C:\Users\u\AppData\Local\Paperboat\runtime\machine-registration.json',
                         fake.calls[1]['payload']['path'])
        winhost._device.cache_clear()

    def test_teammate_environment_is_an_overlay_not_the_controllers_environment(self):
        import teammate_profile
        with patch.dict(os.environ, {'PB_SMOKE_WINDOWS_PB': r'C:\x\pb.exe', 'SECRET_CONTROLLER_VALUE': 'x'}):
            root = winhost.RemotePath(winhost.device(), r'C:\Users\u\.pb-smoke\teammate')
            env = teammate_profile.environment(root)
        self.assertEqual({'XDG_CONFIG_HOME', 'XDG_DATA_HOME', 'XDG_STATE_HOME', 'XDG_CACHE_HOME'}, set(env))
        self.assertEqual(r'C:\Users\u\.pb-smoke\teammate\config', env['XDG_CONFIG_HOME'])

    def test_true_command_depends_on_the_device(self):
        with patch.dict(os.environ, {'PB_SMOKE_WINDOWS_PB': r'C:\x\pb.exe'}):
            self.assertEqual(['cmd', '/c', 'exit', '0'], winhost.true_argv())
        with patch.dict(os.environ, {}, clear=True):
            self.assertEqual(['true'], winhost.true_argv())


if __name__ == '__main__':
    unittest.main()
