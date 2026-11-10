import base64
import json
import os
import pathlib
import subprocess
import sys
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
        script, *chunks = input.decode().split('\n')
        payload = ''.join(chunks[:chunks.index('.')])
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
        self.assertLess(len(command), 2000)  # cmd.exe allows 8191
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
        cmd = pathlib.Path(cls.bin.name) / 'cmd.exe'  # the snippets kill through `cmd /c "taskkill /T /F /PID n >nul 2>&1"`
        cmd.write_text('#!/bin/sh\nkill -9 "$(echo "$2" | sed -E "s/.*PID ([0-9]+).*/\\1/")" 2>/dev/null\nexit 0\n')
        cmd.chmod(0o755)
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

    def test_slow_reader_still_receives_all_output(self):
        import time
        big = pathlib.Path(self.bin.name) / 'big.exe'
        big.write_text('#!/bin/sh\nhead -c 3000000 /dev/zero\n')
        big.chmod(0o755)
        started = time.monotonic()
        result = self.device.run([str(big)], text=False, read_delay=2, timeout=60)
        self.assertEqual(0, result.returncode)
        self.assertEqual(3000000, len(result.stdout))
        self.assertGreaterEqual(time.monotonic() - started, 2)

    def test_remote_exec_reads_events_and_waits_for_its_exit_code(self):
        import exec_cancel
        remote = winhost.job(winhost.WindowsPb(self.device), ['x'])
        try:
            self.assertEqual(0, remote.wait(30))
            self.assertTrue(remote.stdout().startswith(b'args=x'))
            with self.assertRaises(RuntimeError):
                remote.interrupt()
        finally:
            remote.close()

    def test_generated_files_and_child_pid(self):
        import hashlib
        with self.device.temp_dir('pb-test') as directory:
            pattern = directory / 'pattern.bin'
            winhost.write_pattern(pattern, bytes(range(256)) * 4, 5)
            self.assertEqual(bytes(range(256)) * 20, pattern.read_bytes())
            calls = []
            with patch.object(self.device, 'snippet', lambda *a, **payload: calls.append(payload) or (0, b'', b'')):
                winhost.write_pattern(pattern, bytes(range(256)) * 256, 800)  # the checks' 50 MiB file
            self.assertLess(len(calls[0]['block']), 1024)  # victus stopped answering SSH at this step with ~117 KB of stdin
            random = directory / 'random.bin'
            digest = winhost.write_random(random, 3 << 20)
            self.assertEqual(3 << 20, random.size())
            self.assertEqual(hashlib.sha256(random.read_bytes()).hexdigest(), digest)
            sparse = directory / 'sparse.bin'
            winhost.truncate(sparse, 5000)
            self.assertEqual(5000, sparse.size())
            done = self.device.spawn([self.exe, 'x'], directory / 'job')
            done.wait(30, interval=.5)
            self.assertGreater(done.child_pid(), 1)
            self.assertNotEqual(done.pid, done.child_pid())

    def test_origin_serves_its_body(self):
        import urllib.request
        with self.device.origin('synthetic <origin> "body"') as port:
            for _ in range(3):
                with urllib.request.urlopen('http://127.0.0.1:%d/x' % port, timeout=10) as response:
                    self.assertEqual('synthetic <origin> "body"', response.read().decode())


class WindowsChecksRunner(unittest.TestCase):
    def args(self, **values):
        defaults = dict(machine='m', peer='p', team='t', teammate_machine_id='', teammate_account_id='', owner_cmd='',
                        owner_machine_id='', owner_machine_name='', owner_pb='pb')
        return SimpleNamespace(**dict(defaults, **values))

    def test_verdict_needs_exit_zero_and_only_pass_statuses(self):
        import windows_checks
        passed = 'noise\n{"check":"a","status":"PASS"}\n{"row":"C1","status":"PASS"}\n'
        self.assertEqual('PASS', windows_checks.verdict(0, passed))
        self.assertEqual('FAIL', windows_checks.verdict(1, passed))
        self.assertEqual('FAIL', windows_checks.verdict(0, passed + '{"status":"FAIL"}\n'))
        self.assertEqual('FAIL', windows_checks.verdict(0, 'no json at all\n'))
        self.assertEqual('PASS', windows_checks.verdict(0, 'PASS step\nRESULT PASS\n'))
        self.assertEqual('FAIL', windows_checks.verdict(0, 'RESULT FAIL a,b\n'))
        done = windows_checks.run_streaming([sys.executable, '-c', 'import sys;print(\'{"state": "x"}\');print(1);sys.stderr.write("e")'], None, 10)
        self.assertEqual((0, '{"state": "x"}\n1\n', 'e'), (done.returncode, done.stdout, done.stderr))

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


@unittest.skipUnless(PWSH, 'set PB_TEST_PWSH to a pwsh binary to run the target scripts for real')
class WindowsTargetScripts(unittest.TestCase):
    """The PowerShell a Windows TARGET runs in place of python3/sh/printf, executed in a local PowerShell 7."""

    def run_ps(self, script, *arguments, input=b'', env=None):
        import tempfile
        import windows_target
        with tempfile.TemporaryDirectory() as directory:
            argv = windows_target.ps_argv(script)
            argv[0] = PWSH
            return subprocess.run(argv + list(arguments), input=input, capture_output=True,
                                  env=dict(os.environ, **(env or {})))

    def run_file(self, script, *arguments, input=b''):
        import tempfile
        import windows_target
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / 'check.ps1'
            path.write_bytes(b'\xef\xbb\xbf' + windows_target.with_preamble(script).encode())
            return subprocess.run([PWSH, '-NoProfile', '-File', str(path), *arguments], input=input, capture_output=True)

    def test_exec_script_checks_argument_and_stdin_and_writes_exact_output(self):
        import hashlib
        import windows_target
        incoming = 'in λ船\n'.encode() + bytes(range(256))
        argument = 'literal spaces "quotes" λ船 x'
        script = windows_target.exec_script(argument, hashlib.sha256(incoming).hexdigest(), 'PB_UNICODE_λ船\n', 'PB_STDERR_λ船\n', 37)
        done = self.run_file(script, argument, input=incoming)
        self.assertEqual(37, done.returncode)
        self.assertEqual(bytes(range(256)) * 65536 + 'PB_UNICODE_λ船\n'.encode(), done.stdout)
        self.assertEqual('PB_STDERR_λ船\n'.encode(), done.stderr)
        self.assertEqual(91, self.run_file(script, 'other', input=incoming).returncode)
        self.assertEqual(91, self.run_file(script, argument.upper(), input=incoming).returncode)  # exact, not case-folded
        self.assertEqual(92, self.run_file(script, argument, input=b'wrong').returncode)

    def test_small_fixtures(self):
        import windows_target
        self.assertEqual(8 << 20, len(self.run_ps(windows_target.zeros_script(8 << 20)).stdout))
        done = self.run_ps(windows_target.out_err_script('PB_JSON_OUT`n', 'PB_JSON_ERR`n', 19))
        self.assertEqual((19, b'PB_JSON_OUT\n', b'PB_JSON_ERR\n'), (done.returncode, done.stdout, done.stderr))
        self.assertEqual(b"it's-marker", self.run_ps(windows_target.print_script("it's-marker")).stdout)
        woken = self.run_file(windows_target.sleeper_script().replace('Start-Sleep -Seconds 120', ''), 'nonce')
        self.assertRegex(woken.stdout.decode(), r'^nonce \d+\n$')

    def test_inbox_scripts_verify_and_clean_exactly_their_files(self):
        import hashlib
        import tempfile
        import windows_target
        with tempfile.TemporaryDirectory() as home:
            inbox = pathlib.Path(home) / 'Paperboat' / 'Inbox'
            inbox.mkdir(parents=True)
            expected = {}
            for name, data in (('smoke-a.bin', b'aaa'), ('smoke-b.bin', b'bbb')):
                (inbox / name).write_bytes(data)
                expected[name] = hashlib.sha256(data).hexdigest()
            (inbox / 'keep.txt').write_text('mine')
            env = {'USERPROFILE': home}
            ok = self.run_ps(windows_target.inbox_verify_script(expected, 'VERIFIED', 3, 'smoke-'), env=env)
            self.assertEqual((0, b'VERIFIED\n'), (ok.returncode, ok.stdout))
            shouted = dict(expected, **{'smoke-a.bin': expected['smoke-a.bin'].upper()})
            self.assertNotEqual(0, self.run_ps(windows_target.inbox_verify_script(shouted, 'V'), env=env).returncode)
            bad = dict(expected, **{'smoke-a.bin': '0' * 64})
            self.assertNotEqual(0, self.run_ps(windows_target.inbox_verify_script(bad, 'VERIFIED'), env=env).returncode)
            self.assertNotEqual(0, self.run_ps(windows_target.inbox_verify_script(expected, 'V', 4), env=env).returncode)
            self.assertNotEqual(0, self.run_ps(windows_target.inbox_cleanup_script('other-', ['smoke-a.bin']), env=env).returncode)
            self.assertEqual(0, self.run_ps(windows_target.inbox_cleanup_script('smoke-', ['smoke-a.bin']), env=env).returncode)
            self.assertEqual(['keep.txt', 'smoke-b.bin'], sorted(p.name for p in inbox.iterdir()))
            self.assertEqual(0, self.run_ps(windows_target.inbox_cleanup_glob_script('smoke-'), env=env).returncode)
            self.assertEqual(['keep.txt'], sorted(p.name for p in inbox.iterdir()))


class WindowsTargetPlumbing(unittest.TestCase):
    def test_is_windows_reads_the_machine_platform_once(self):
        import windows_target
        windows_target._platforms.clear()
        listing = {'machines': [{'id': 'machine_1', 'alias': 'victus', 'platform': 'windows'},
                                {'id': 'machine_2', 'alias': 'hp', 'platform': 'linux'}]}
        with patch('pairing_lifecycle.invoke', return_value=listing) as invoke:
            self.assertTrue(windows_target.is_windows('pb', 'victus'))
            self.assertTrue(windows_target.is_windows('pb', 'victus'))
            self.assertFalse(windows_target.is_windows('pb', 'machine_2'))
            self.assertFalse(windows_target.is_windows('pb', 'unknown'))
        self.assertEqual(3, invoke.call_count)
        windows_target._platforms.clear()

    def test_staged_writes_and_removes_its_file_even_when_the_check_fails(self):
        import windows_target
        calls = []

        def run(binary, *arguments, **kwargs):
            calls.append(arguments)
            return (b'C:\\Temp\\pb-smoke-1.ps1', b'')

        with patch('exec_lifecycle.run', run):
            with self.assertRaises(KeyError):
                with windows_target.staged('pb', 'victus', 'exit 3') as path:
                    self.assertEqual('C:\\Temp\\pb-smoke-1.ps1', path)
                    raise KeyError
        self.assertEqual('exec', calls[0][0])
        self.assertEqual(2, len(calls))
        self.assertEqual(['powershell', '-NoProfile'], list(calls[1][5:7]))

    def test_exact_comparisons_are_case_sensitive(self):
        import windows_target
        for script in (windows_target.stopped_script(1, 'Nonce'), windows_target.kill_script(1, 'Nonce')):
            self.assertRegex(script, r'-c(not)?match')
            self.assertNotRegex(script, r'-(not)?match')

    def test_target_pb_prefers_the_environment(self):
        import windows_target
        with patch.dict(os.environ, {'PB_SMOKE_TARGET_WINDOWS_PB': r'C:\x\pb.exe'}):
            self.assertEqual(r'C:\x\pb.exe', windows_target.target_pb('pb', 'victus'))


class SessionFamily(unittest.TestCase):
    def test_terminal_argv_is_plain_on_unix_and_ssh_tt_on_windows(self):
        self.assertEqual(['/bin/pb', 'connect', 'hp'], winhost.terminal_argv(pathlib.Path('/bin/pb'), ['connect', 'hp']))
        device = winhost.WindowsDevice(target='user@host', pb_exe=EXE, password='secret')
        with patch.dict(os.environ, {}, clear=True):
            argv = winhost.terminal_argv(winhost.WindowsPb(device), ['connect', 'my box', '--name', "it's"])
            self.assertNotIn('SSHPASS', os.environ)  # only the harness child gets it
        self.assertEqual({'SSHPASS': 'secret'}, winhost.terminal_env(winhost.WindowsPb(device)))
        self.assertEqual({}, winhost.terminal_env(pathlib.Path('/bin/pb')))
        self.assertEqual(['sshpass', '-e', 'ssh'], argv[:3])
        self.assertIn('-tt', argv)
        self.assertNotIn('secret', ' '.join(argv))
        command = argv[-1]
        self.assertIn("& 'C:\\Program Files\\Paperboat\\bin\\pb.exe' 'connect' 'my box' '--name' 'it''s'", command)

    def test_every_terminal_argv_harness_gets_the_password_environment(self):
        # Without SSHPASS the pty child is sshpass printing its usage: the terminal closes before any output.
        for path in pathlib.Path(__file__).parent.glob('*_*.py'):
            for line in [] if path.name.startswith('test_') else path.read_text().splitlines():
                if '(winhost.terminal_argv(' in line:
                    self.assertIn('env=winhost.terminal_env(', line, path.name)

    def test_session_cleanup_reads_the_device_receipt(self):
        import session_cleanup

        class Remote:
            def run_remote(self, arguments, timeout, text):
                self.arguments = arguments
                return SimpleNamespace(returncode=0, stdout=b'{"version":"1","sessions":[]}')

        remote = Remote()
        self.assertEqual((0, {'version': '1', 'sessions': []}), session_cleanup.command(remote, 'list', 'hp'))
        self.assertEqual(['session', 'list', 'hp', '--json'], remote.arguments)


class LocalTransferHelpers(unittest.TestCase):
    def test_local_files_match_the_remote_contract(self):
        import hashlib
        with winhost.workdir('pb-test') as directory:
            winhost.write_pattern(directory / 'p', b'ab', 3)
            self.assertEqual(b'ababab', (directory / 'p').read_bytes())
            digest = winhost.write_random(directory / 'r', 2 << 20)
            self.assertEqual(hashlib.sha256((directory / 'r').read_bytes()).hexdigest(), digest)
            winhost.truncate(directory / 's', 100)
            self.assertEqual(100, (directory / 's').stat().st_size)
            self.assertIsInstance(directory, pathlib.Path)

    def test_run_pb_and_job_pick_the_side(self):
        out = winhost.run_pb(sys.executable, ['-c', 'print(7)'], 10)
        self.assertEqual(b'7\n', out.stdout)
        job = winhost.job(sys.executable, ['-c', 'print(8)'])
        try:
            self.assertEqual(0, job.wait(10))
            self.assertEqual(b'8\n', job.stdout())
        finally:
            job.close()


class ExecFamily(unittest.TestCase):
    def test_events_and_local_exec(self):
        import exec_cancel
        self.assertEqual([{'a': 1}, {'b': 2}], exec_cancel.events(b'{"a":1}\n{"b":2}\n{"partial'))
        local = winhost.LocalJob([sys.executable, '-c', 'import signal,time;signal.signal(signal.SIGINT,signal.SIG_DFL);print("hello",flush=True);time.sleep(30)'])
        try:
            import time
            for _ in range(50):
                if local.stdout():
                    break
                time.sleep(.1)
            self.assertEqual(b'hello\n', local.stdout())
            self.assertIsNone(local.poll())
            local.interrupt()
            self.assertEqual(-2, local.wait(10))
        finally:
            local.close()


class PreviewFamily(unittest.TestCase):
    def test_exposed_is_the_identity_on_unix_and_an_ssh_forward_on_windows(self):
        import preview_lifecycle
        with patch.dict(os.environ, {}, clear=True), preview_lifecycle.exposed(1234) as port:
            self.assertEqual(1234, port)
        calls = []

        class Tunnel:
            def poll(self):
                return None

            def terminate(self):
                calls.append('terminated')

            def wait(self, timeout):
                pass

        def popen(argv, **kwargs):
            calls.append(argv)
            return Tunnel()

        device = winhost.WindowsDevice(target='user@host', pb_exe=EXE, password='secret')
        with patch.dict(os.environ, {'PB_SMOKE_WINDOWS_PB': EXE}), patch('subprocess.Popen', popen), \
                patch.object(winhost, '_device', lambda exe: device), \
                patch.object(device, 'listening', lambda port: True), preview_lifecycle.exposed(4321) as port:
            self.assertTrue(20000 <= port < 60000)
            forward = calls[0]
            self.assertEqual(['sshpass', '-e', 'ssh'], forward[:3])
            self.assertIn('127.0.0.1:%d:127.0.0.1:4321' % port, forward)
            self.assertIn('ExitOnForwardFailure=yes', forward)
            self.assertEqual('user@host', forward[-1])
            self.assertNotIn('secret', ' '.join(forward))
        self.assertEqual('terminated', calls[-1])

    def test_forward_gives_up_when_ssh_cannot_forward(self):
        class Dead:
            def poll(self):
                return 255

            def terminate(self):
                pass

        device = winhost.WindowsDevice(target='user@host', pb_exe=EXE, password=None)
        with patch('subprocess.Popen', lambda *a, **k: Dead()):
            with self.assertRaisesRegex(winhost.DeviceError, 'is TCP forwarding allowed'):
                with device.forward(1):
                    pass

    def test_ported_family_uses_the_shared_origin_and_device_pb(self):
        for name in ('tunnel_manage', 'tunnel_lifecycle', 'preview_manage', 'preview_access', 'preview_lifecycle',
                     'preview_inspect', 'preview_protocols', 'tunnel_public_protocols'):
            source = (pathlib.Path(__file__).parent / (name + '.py')).read_text()
            self.assertNotIn(".local/bin/pb'", source, name)
            self.assertIn('winhost', source, name)



def shell_words(line):
    """argv of a command line typed at a Windows prompt, with PowerShell taken from PB_TEST_PWSH."""
    import shlex
    words = shlex.split(line)
    assert words[0] == 'powershell'
    return [PWSH] + words[1:]


@unittest.skipUnless(PWSH, 'set PB_TEST_PWSH to a pwsh binary to run the SSH snippets for real')
class SshFamilyInPowerShell(unittest.TestCase):
    def run_line(self, line, env=None):
        return subprocess.run(shell_words(line), capture_output=True, env=dict(os.environ, **(env or {})))

    def test_tree_hashes_and_which_on_the_device(self):
        import hashlib
        device = SnippetsInPowerShell.device if hasattr(SnippetsInPowerShell, 'device') else None
        if device is None:
            SnippetsInPowerShell.setUpClass()
            device = SnippetsInPowerShell.device
        with device.temp_dir('pb-test') as directory:
            self.assertEqual({}, directory.tree_hashes())
            (directory / 'data.bin').write_bytes(bytes(range(256)))
            (directory / 'nested' / 'λ船.txt').write_text('exact Unicode λ船\n')
            (directory / 'empty').write_bytes(b'')
            self.assertEqual({'data.bin': hashlib.sha256(bytes(range(256))).hexdigest(), 'empty': hashlib.sha256(b'').hexdigest(),
                              'nested/λ船.txt': hashlib.sha256('exact Unicode λ船\n'.encode()).hexdigest()}, directory.tree_hashes())
        with patch.object(winhost, 'device', lambda: device):
            self.assertEqual('sh', pathlib.PurePosixPath(str(winhost.which('sh'))).name)
            self.assertTrue(hasattr(winhost.which('sh'), 'run_remote'))
            self.assertIsNone(winhost.which('pb-no-such-tool'))

    def test_ssh_command_is_one_line_cmd_passes_through_unchanged(self):
        import windows_target
        line = windows_target.ssh_command(windows_target.exact_output_script('m_λ船\n', 'PB_SSH_STDERR\n', 37))
        self.assertNotRegex(line, r'["\'&|<>^%\n]')  # nothing cmd.exe or a POSIX shell would interpret
        done = self.run_line(line)
        self.assertEqual((37, 'm_λ船\n'.encode(), b'PB_SSH_STDERR\n'), (done.returncode, done.stdout, done.stderr))

    def test_typed_marker_prints_the_token_its_own_text_does_not_contain(self):
        import windows_target
        line = windows_target.typed_marker('PB_SSH_READY_abc')
        self.assertNotIn('PB_SSH_READY_abc', line)
        self.assertEqual(b'PB_SSH_READY_abc', self.run_line(line).stdout.strip())
        line = windows_target.typed_marker('PB_KEY_', "+$env:PB_MARK+'_k1'")
        self.assertEqual(b'PB_KEY_m7_k1', self.run_line(line, {'PB_MARK': 'm7'}).stdout.strip())
        with self.assertRaises(AssertionError):
            windows_target.typed_marker("x'; exit")

    def test_username_tools_directory_and_long_scripts(self):
        import tempfile
        import windows_target
        run = lambda script, env=None: self.run_line(windows_target.ssh_command(script), env)  # noqa: E731
        self.assertEqual(b'pb user', run(windows_target.username_script(), {'USERNAME': 'pb user'}).stdout)
        name = 'smoke-ssh-tools-' + '0' * 32
        with tempfile.TemporaryDirectory() as home:
            env = {'USERPROFILE': home}
            made = run(windows_target.tools_setup_script(name), env)
            self.assertEqual((0, b'created'), (made.returncode, made.stdout))
            self.assertTrue((pathlib.Path(home) / 'Paperboat' / name / 'scp').is_dir())
            self.assertNotEqual(0, run(windows_target.tools_setup_script(name), env).returncode)  # exist_ok=False
            (pathlib.Path(home) / 'Paperboat' / 'keep').write_text('mine')
            self.assertEqual(0, run(windows_target.tools_cleanup_script(name), env).returncode)
            self.assertEqual(['keep'], [p.name for p in (pathlib.Path(home) / 'Paperboat').iterdir()])
        done = run(windows_target.long_script(2, 'PB_SSH_LONG_x', step=1))
        self.assertEqual((0, b't1\nt2\nPB_SSH_LONG_x\n'), (done.returncode, done.stdout))


class SshFamilyPlumbing(unittest.TestCase):
    def test_tools_directory_names_are_only_this_checks(self):
        import windows_target
        for bad in ('Inbox', '..', 'smoke-ssh-tools-xyz', "smoke-ssh-tools-" + '0' * 32 + "'"):
            with self.assertRaises(ValueError):
                windows_target.tools_cleanup_script(bad)

    def test_windows_pb_runs_another_executable_and_long_script_defaults_to_a_minute(self):
        import windows_target
        device = winhost.WindowsDevice(target='user@host', pb_exe=EXE, password='secret')
        fake = FakeSsh((0, envelope(0, b'ok')))
        with patch('subprocess.run', fake):
            tool = winhost.WindowsPb(device, r'C:\Windows\System32\OpenSSH\ssh.exe')
            self.assertEqual('ok', tool.run_remote(['-V']).stdout)
        self.assertEqual(r'C:\Windows\System32\OpenSSH\ssh.exe', fake.calls[0]['payload']['exe'])
        self.assertEqual(EXE, str(winhost.WindowsPb(device)))
        self.assertIn('Start-Sleep -Seconds 60;$i+=60', windows_target.long_script(3601, 'M'))

    def test_doctor_hides_openssh_by_path_on_unix_and_by_its_override_on_windows(self):
        import ssh_doctor_lifecycle
        ready = {'machine': {'alias': 'peer'}, 'openssh': {'ready': True}, 'local_configuration': 'ready',
                 'managed_agent': 'ready', 'transport': 'ready', 'target': {'state': 'ready', 'port': 22},
                 'host_identity': 'k', 'managed_keys': 'reconciled'}
        refused = SimpleNamespace(returncode=1, args=[], stderr='', stdout=json.dumps(
            {'error': {'message': 'A supported OpenSSH client is unavailable. Install OpenSSH'}}))
        for windows in (False, True):
            seen = {}

            def result(binary, *arguments, env, timeout):
                seen.update(env=env)
                return refused

            import contextlib
            directory = winhost.RemotePath(winhost.WindowsDevice(pb_exe=EXE, password=''), r'C:\T\d') if windows else pathlib.Path('/tmp/d')
            with patch.dict(os.environ, {'PB_SMOKE_WINDOWS_PB': EXE} if windows else {'CONTROLLER_ONLY': '1'}, clear=True), \
                    patch.object(ssh_doctor_lifecycle, 'registration', lambda: 1), \
                    patch.object(ssh_doctor_lifecycle, 'invoke', lambda *a, **k: ready), \
                    patch.object(ssh_doctor_lifecycle, 'command_result', result), \
                    patch.object(winhost, 'workdir', lambda prefix: contextlib.nullcontext(directory)), \
                    patch('sys.argv', ['x', 'peer']), patch('builtins.print'):
                ssh_doctor_lifecycle.main()
            if windows:  # an overlay for the device: never the controller's environment
                self.assertEqual({'PAPERBOAT_OPENSSH_PATH': r'C:\T\d\ssh.exe'}, seen['env'])
            else:
                self.assertEqual({'CONTROLLER_ONLY': '1', 'PATH': '/tmp/d'}, seen['env'])

    def tools_run(self, windows):
        """ssh_tools_lifecycle.main() against a fake pb whose scp/sftp/rsync copy inside a local directory."""
        import shlex
        import shutil
        import tempfile
        import ssh_tools_lifecycle
        import windows_target
        calls = []
        with tempfile.TemporaryDirectory() as remote_home:
            home = pathlib.Path(remote_home)

            def local(operand):
                if not operand.startswith('peer:'):
                    return pathlib.Path(operand)
                return home / operand[5:] if windows else pathlib.Path(operand[5:])

            def run(binary, tool, *arguments, **_):
                calls.append((tool, arguments))
                if tool == 'ssh' and windows:  # the PowerShell itself runs for real in SshFamilyInPowerShell
                    if len(calls) == 1:
                        (home / 'Paperboat' / self.fixture / 'scp').mkdir(parents=True)
                    else:
                        shutil.rmtree(home / 'Paperboat' / self.fixture)
                    return b'created', b''
                if tool == 'ssh':
                    code = shlex.split(arguments[2])[2]
                    done = subprocess.run([sys.executable, '-c', code], capture_output=True, env=dict(os.environ, HOME=remote_home))
                    self.assertEqual(0, done.returncode, done.stderr)
                    return done.stdout, b''
                if tool == 'scp':
                    source, target = local(arguments[1]), local(arguments[2])
                    shutil.copytree(source, target / source.name if str(arguments[2]).endswith('/') else target)
                elif tool == 'rsync':
                    shutil.copytree(local(arguments[1]), local(arguments[2]), dirs_exist_ok=True)
                elif tool == 'sftp':
                    for line in pathlib.Path(arguments[1]).read_text().splitlines():
                        verb, *paths = shlex.split(line)
                        remote = lambda p: home / p if windows else pathlib.Path(p)  # noqa: E731
                        if verb == 'put':
                            shutil.copyfile(paths[0], remote(paths[1]))
                        elif verb == 'get':
                            shutil.copyfile(remote(paths[0]), paths[1])
                        else:
                            remote(paths[0]).unlink()
                return b'', b''

            (home / 'Paperboat').mkdir()
            fixed = SimpleNamespace(hex='ab' * 16)
            self.fixture = 'smoke-ssh-tools-' + fixed.hex
            printed = []
            with patch.object(ssh_tools_lifecycle, 'run', run), patch.object(ssh_tools_lifecycle, 'registration', lambda: 1), \
                    patch.object(windows_target, 'is_windows', lambda binary, target: windows), \
                    patch.object(ssh_tools_lifecycle.uuid, 'uuid4', lambda: fixed), \
                    patch.dict(os.environ, {'HOME': os.environ['HOME']}, clear=True), \
                    patch('sys.argv', ['x', 'peer']), patch('builtins.print', printed.append):
                ssh_tools_lifecycle.main()
            return calls, json.loads(printed[-1]), sorted(p.name for p in (home / 'Paperboat').iterdir())

    def test_tools_round_trip_to_a_unix_target_keeps_rsync_and_the_python_fixture(self):
        calls, receipt, left = self.tools_run(windows=False)
        self.assertEqual(['ssh', 'scp', 'scp', 'sftp', 'rsync', 'rsync', 'ssh'], [tool for tool, _ in calls])
        self.assertTrue(calls[0][1][2].startswith('python3 -c '))
        self.assertEqual(['scp', 'sftp', 'rsync'], receipt['tools'])
        self.assertEqual([], left)

    def test_tools_round_trip_to_a_windows_target_uses_powershell_and_home_relative_paths(self):
        calls, receipt, left = self.tools_run(windows=True)
        self.assertEqual([], left)
        self.assertEqual(['ssh', 'scp', 'scp', 'sftp', 'ssh'], [tool for tool, _ in calls])
        self.assertTrue(calls[0][1][2].startswith('powershell -NoProfile '))
        self.assertEqual('peer:Paperboat/' + self.fixture + '/scp/', calls[1][1][2])
        self.assertEqual(['scp', 'sftp'], receipt['tools'])



# Stands in for cmd.exe in a pty: a prompt, `a & b`, `set "N=V"`, exit, and Ctrl-C that ends only the foreground
# program. Each `powershell -NoProfile -Command "<script>"` it is given runs for real in PB_TEST_PWSH.
FAKE_CMD = r"""
import os, re, signal, subprocess, sys
signal.signal(signal.SIGINT, signal.SIG_IGN)
env = dict(os.environ, LC_ALL='C.UTF-8')
while True:
    sys.stdout.write('C:\\>'); sys.stdout.flush()
    line = sys.stdin.readline()
    if not line or line.strip() == 'exit':
        break
    for part in line.strip().split(' & '):
        setting = re.fullmatch(r'set "(\w+)=(.*)"', part)
        script = re.fullmatch(r'powershell -NoProfile -Command "(.*)"', part)
        if setting:
            env[setting[1]] = setting[2]
        elif script:
            subprocess.run([sys.argv[1], '-NoProfile', '-Command', script[1]], env=env,
                           preexec_fn=lambda: signal.signal(signal.SIGINT, signal.SIG_DFL))
        elif part:
            print('not a command of this fixture: ' + part)
"""


@unittest.skipUnless(PWSH, 'set PB_TEST_PWSH to a pwsh binary to run the terminal lines for real')
class TerminalFamilyInPowerShell(unittest.TestCase):
    def run_line(self, line, input=None):
        return subprocess.run(shell_words(line), capture_output=True, input=input)

    def test_typed_lines_print_what_the_sh_commands_print(self):
        import windows_target
        self.assertEqual(b'LR1\nLR2\nLR3\n', self.run_line(windows_target.count_line('LR', 3)).stdout)
        self.assertEqual(b'SW1\nSW2\n', self.run_line(windows_target.count_line('SW', 2, slow=True)).stdout)
        flood = self.run_line(windows_target.flood_line(3)).stdout
        self.assertEqual([b'0123456789012345678901234567890123456789012345678'] * 3, flood.split())
        self.assertEqual(b'', self.run_line(windows_target.sink_line(), input=b'x' * 99 + b'\n' + b'xx\n').stdout)
        self.assertEqual(b'\x1b[31mPB_COLOUR\x1b[0m', self.run_line(windows_target.colour_line()).stdout.strip())
        self.assertEqual(b'PB_FOREGROUND_abc', self.run_line(windows_target.foreground_line('PB_FOREGROUND_abc', 0)).stdout.strip())
        self.assertNotIn('PB_FOREGROUND_abc', windows_target.foreground_line('PB_FOREGROUND_abc', 0))
        self.assertEqual(b'PB_ARITH_42', self.run_line(windows_target.typed_marker('PB_ARITH_', '+(6*7)')).stdout.strip())
        for bad in ('say "x"', 'echo 100%', 'a\nb'):
            with self.assertRaises(AssertionError):
                windows_target.typed_line(bad)

    def test_count_in_files_reads_files_another_process_writes(self):
        SnippetsInPowerShell.setUpClass()
        device = SnippetsInPowerShell.device
        with device.temp_dir('pb-test') as directory:
            self.assertEqual(0, device.count_in_files(directory, 'events-*.ndjson', 'peer_stream_opened'))
            (directory / 'events-1.ndjson').write_text('{"e":"peer_stream_opened"}\n{"e":"other"}\n{"e":"peer_stream_opened"}\n')
            (directory / 'events-2.ndjson').write_text('{"e":"peer_stream_opened"}\n')
            (directory / 'other.log').write_text('peer_stream_opened')
            with open(str(directory / 'events-2.ndjson'), 'a'):  # held open for writing, as the daemon does
                self.assertEqual(3, device.count_in_files(directory, 'events-*.ndjson', 'peer_stream_opened'))

    def fake_cmd(self):
        import tempfile
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        script = pathlib.Path(directory.name) / 'cmd.py'
        script.write_text(FAKE_CMD)
        return [sys.executable, str(script), PWSH]

    def test_terminal_lifecycle_windows_phases_against_a_console_that_echoes(self):
        import terminal_lifecycle
        terminal = terminal_lifecycle.Terminal(self.fake_cmd())
        try:
            phases = terminal_lifecycle.windows_phases(terminal, 'main', 'off')
        finally:
            terminal.close()
        self.assertEqual(['input_output', 'unicode', 'resize', 'colour', 'ctrl_c', 'exit'], phases)
        self.assertEqual(0, terminal.status)
        for check in ('paste', 'typing', 'apps', 'monitor'):
            with self.assertRaisesRegex(RuntimeError, 'no Windows form'):
                terminal_lifecycle.windows_phases(None, check, 'off')

    def test_matrix_client_types_powershell_lines_at_a_windows_session(self):
        import terminal_matrix
        import windows_target
        with patch.object(terminal_matrix, 'TARGET', 'victus'), \
                patch.dict(windows_target._platforms, {(str(terminal_matrix.PB), 'victus'): True}):
            client = terminal_matrix.Client(self.fake_cmd())
            try:
                client.resize(31, 97)
                client.ready()
                self.assertEqual((31, 97), client.size())
                out = client.run(terminal_matrix.count('LR', 5))
                # pwsh on Linux starts its first line with a keypad escape, so the numbers are read without line anchors
                self.assertEqual([b'1', b'2', b'3', b'4', b'5'], __import__('re').findall(rb'LR(\d+)', out))
                client.run(terminal_matrix.set_mark('m7'))
                client.send(terminal_matrix.say_mark('PB_KEY_', 'k1') + '\r')
                client.expect('PB_KEY_m7_k1')
                client.run(terminal_matrix.noop())
                self.assertEqual(1, bytes(client.output).count(b'PB_KEY_m7_k1'))  # the echo of the typed line is not a hit
            finally:
                client.close()


class TerminalFamilyPlumbing(unittest.TestCase):
    def test_unix_command_strings_are_unchanged(self):
        import terminal_matrix as matrix
        self.assertIsNone(matrix.TARGET)  # importers of connect() (terminal_dead_machine) never ask for the platform
        self.assertEqual("printf '\\nPB_BOTH_%s\\n' 'tok'", matrix.say('PB_BOTH_', 'tok'))
        self.assertEqual("printf '\\nPB_KEY_%s_%s\\n' \"$PB_RESTART_MARK\" 'k'", matrix.say_mark('PB_KEY_', 'k'))
        self.assertEqual('export PB_RESTART_MARK=m', matrix.set_mark('m'))
        self.assertEqual("seq -f 'LR%g' 1 2000", matrix.count('LR', 2000))
        self.assertEqual('for i in $(seq 1 90); do echo SW$i; sleep 1; done', matrix.count('SW', 90, slow=True))
        self.assertEqual('true', matrix.noop())
        sent = []
        client = object.__new__(matrix.Client)
        client.send, client.expect = sent.append, lambda token, timeout: token
        token = client.run('stty size')[8:]
        self.assertEqual(["stty size; printf '\\nPB_DONE_%s\\n' '" + token + "'\r"], sent)

    def test_windows_target_strings_and_rows_without_an_equivalent(self):
        import terminal_matrix as matrix
        import windows_target
        with patch.object(matrix, 'TARGET', 'victus'), patch.dict(windows_target._platforms, {(str(matrix.PB), 'victus'): True}), \
                patch.object(matrix, 'RESULTS', []), patch('builtins.print'):
            self.assertEqual('set "PB_RESTART_MARK=m"', matrix.set_mark('m'))
            sent = []
            client = object.__new__(matrix.Client)
            client.send, client.expect = sent.append, lambda token, timeout: token
            client.run(matrix.set_mark('m'))
            client.run(matrix.noop())
            self.assertRegex(sent[0], r'^set "PB_RESTART_MARK=m" & powershell -NoProfile -Command ".*"\r$')
            self.assertRegex(sent[1], r'^powershell -NoProfile -Command ".*"\r$')
            self.assertFalse(matrix.target_darwin('victus'))  # without running uname there
            matrix.reconnect_keeps_control(SimpleNamespace(target='victus'), [])
            matrix.large_paste(SimpleNamespace(target='victus'), [])
            self.assertEqual(['SKIP', 'SKIP'], [state for _, state in matrix.RESULTS])

    def test_device_clients_are_found_by_their_command_line_and_firewall_rows_skip(self):
        import terminal_matrix as matrix

        class Device:
            calls = []

            def processes(self, pattern):
                self.calls.append(('processes', pattern))
                return [{'ProcessId': 11, 'Name': 'powershell.exe'}, {'ProcessId': 12, 'Name': 'pb.exe'}]

            def __getattr__(self, name):
                return lambda pid: self.calls.append((name, pid))

        device = Device()
        with patch.object(matrix, 'DEVICE', device), patch.object(matrix, 'RESULTS', []), patch('builtins.print'):
            client = object.__new__(matrix.Client)
            client.key = 'smoke-matrix-0123456789'
            client.suspend(), client.resume(), client.kill()
            self.assertEqual([('suspend', 12), ('resume', 12), ('stop', 12)], [c for c in device.calls if c[0] != 'processes'])
            self.assertEqual(('processes', 'smoke\\-matrix\\-0123456789'), device.calls[0])
            self.assertFalse(matrix.has_iptables())
            matrix.echo_latency(SimpleNamespace(target='hp'), [])
            self.assertEqual(['SKIP'], [state for _, state in matrix.RESULTS])
            device.processes = lambda pattern: []
            with self.assertRaisesRegex(RuntimeError, 'expected one pb.exe'):
                client.suspend()

    def test_pb_run_checks_the_exit_code_when_asked(self):
        import terminal_matrix as matrix
        with patch.object(matrix, 'PB', sys.executable):
            self.assertEqual('7\n', matrix.pb_run('-c', 'print(7)').stdout)
            self.assertEqual(3, matrix.pb_run('-c', 'raise SystemExit(3)').returncode)
            with self.assertRaises(subprocess.CalledProcessError):
                matrix.pb_run('-c', 'raise SystemExit(3)', check=True)

    def test_lifecycle_scripts_take_pb_and_their_terminal_from_winhost(self):
        for name in ('terminal_lifecycle', 'terminal_matrix', 'ssh_lifecycle', 'ssh_tools_lifecycle', 'ssh_doctor_lifecycle',
                     'ssh_alias_lifecycle', 'ssh_long'):
            source = (pathlib.Path(__file__).parent / (name + '.py')).read_text()
            self.assertNotIn("Path.home() / '.local/bin/pb'", source, name)
            self.assertIn('winhost.pb_path()', source, name)



class PrivateAccessFamily(unittest.TestCase):
    def echo(self, marker):
        import socketserver
        import threading
        import private_access
        server = socketserver.ThreadingTCPServer(('127.0.0.1', 0), private_access.Echo)
        server.daemon_threads, server.marker = True, marker
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        return server.server_address[1]

    @unittest.skipUnless(PWSH, 'set PB_TEST_PWSH to a pwsh binary to run the exchange snippet for real')
    def test_device_exchange_sends_the_payload_half_closes_and_reads_the_reply(self):
        import private_access
        device = LocalDevice(pb_exe='/x/pb.exe', password='')
        port = self.echo('PB_PRIVATE_m1')
        with patch.object(winhost, 'device', lambda: device):
            private_access.exchange(('127.0.0.1', port), {'marker': 'PB_PRIVATE_m1'})
            with self.assertRaises(private_access.ExchangeMismatch):
                private_access.exchange(('127.0.0.1', port), {'marker': 'PB_PRIVATE_other'})
            import socket
            with socket.socket() as closed:
                closed.bind(('127.0.0.1', 0))
                free = closed.getsockname()[1]
            with self.assertRaises(ConnectionRefusedError) as refused:
                private_access.exchange(('127.0.0.1', free), {'marker': 'x'})
            self.assertEqual(__import__('errno').ECONNREFUSED, refused.exception.errno)  # the probe retries on this

    def test_device_curl_waits_for_the_port_listener_and_never_hides_another_failure(self):
        import private_access
        for codes, wait, want in [([7, 7, 0], 45, 0), ([7, 7], 0, 7), ([60, 0], 45, 60)]:
            left = list(codes)
            run = lambda *_: SimpleNamespace(returncode=left.pop(0))
            with patch.object(winhost, 'which', lambda name: name), patch.object(winhost, 'run_pb', run):
                self.assertEqual(want, private_access.device_curl(['https://x.example.test/'], wait, 0).returncode)

    def test_peer_origin_answers_the_unix_exchange_and_the_https_get(self):
        import hashlib
        import urllib.request
        import private_access
        for check in ('access-machine', 'private-https'):
            origin = subprocess.Popen([sys.executable, '-c', private_access.PEER_ORIGIN, check, 'PB_PRIVATE_m2'],
                                      stdout=subprocess.PIPE)
            try:
                port = int(origin.stdout.readline())
                if check == 'private-https':
                    with urllib.request.urlopen('http://127.0.0.1:%d/' % port, timeout=10) as response:
                        self.assertEqual(b'PB_PRIVATE_m2' + hashlib.sha256(private_access.PAYLOAD).digest(), response.read())
                else:
                    private_access.exchange(('127.0.0.1', port), {'marker': 'PB_PRIVATE_m2'})
            finally:
                origin.kill()
                origin.wait()
                origin.stdout.close()

    def test_origin_publishes_the_forwarded_port_of_the_device(self):
        import contextlib
        import threading
        import private_access
        printed, entered = [], []

        @contextlib.contextmanager
        def exposed(port):
            entered.append(port)
            yield 54321
            entered.append('closed')

        with patch.object(private_access, 'exposed', exposed), patch.object(private_access, 'registration', lambda: {'machine_id': 'machine_w'}), \
                patch('builtins.print', lambda line, **_: printed.append(json.loads(line))):
            worker = threading.Thread(target=private_access.origin, args=(SimpleNamespace(check='access-machine', target='victus'),))
            worker.start()
            for _ in range(100):
                if printed:
                    break
                __import__('time').sleep(.05)
            phase = printed[0]
            self.assertEqual((54321, 'machine_w', 'private_access_required'), (phase['port'], phase['machine_id'], phase['state']))
            self.assertNotEqual(54321, entered[0])  # the origin's own port stays on the controller
            pathlib.Path(phase['gate']).touch()
            worker.join(10)
        self.assertEqual('closed', entered[-1])
        self.assertEqual('PASS', printed[-1]['status'])

    def job(self, stdout, returncode=None):
        calls = []
        fake = SimpleNamespace(stdout=lambda limit=None: stdout, stderr=lambda: b'', poll=lambda: returncode,
                               kill=lambda: calls.append('kill'), close=lambda: calls.append('close'))
        return fake, calls

    def test_forward_on_the_device_reads_the_listener_from_the_job_and_always_ends_it(self):
        import private_access
        phase = {'machine_id': 'machine_p', 'port': 8080, 'marker': 'm'}
        receipt = json.dumps({'data': {'schema': 'paperboat.machine-access/v1', 'machine_id': 'machine_p', 'remote_port': 8080,
                                       'listen_address': '127.0.0.1:41000'}}).encode() + b'\n'
        binary = winhost.WindowsPb(winhost.WindowsDevice(pb_exe=EXE, password=''))
        started, exchanged = [], []
        fake, calls = self.job(receipt)
        with patch.object(winhost, 'job', lambda b, arguments: started.append(arguments) or fake), \
                patch.object(private_access, 'exchange', lambda address, p: exchanged.append(address)):
            private_access.forward(binary, phase)
        self.assertEqual(['access', 'machine', 'machine_p', '--port', '8080', '--listen', '127.0.0.1:0', '--json'], started[0])
        self.assertEqual([('127.0.0.1', 41000)], exchanged)
        self.assertEqual(['kill', 'close'], calls)
        fake, calls = self.job(b'', returncode=1)
        with patch.object(winhost, 'job', lambda b, arguments: fake), \
                patch.object(private_access, 'failure_diagnostic', lambda *a: '/private/receipt'):
            with self.assertRaisesRegex(RuntimeError, 'private diagnostics: /private/receipt'):
                private_access.forward(binary, phase)
        self.assertEqual(['kill', 'close'], calls)

    def test_pair_starts_the_origin_on_the_peer_and_probes_from_the_device(self):
        import base64 as b64
        import private_access
        started, probed = [], []
        fake, calls = self.job(b'40123\n')
        machines = {'machines': [{'id': 'machine_p', 'alias': 'hp'}, {'id': 'machine_w', 'alias': 'victus'}]}
        args = SimpleNamespace(target='hp', check='private-address', phase=None)
        with patch.dict(os.environ, {'PB_SMOKE_WINDOWS_PB': EXE}), patch.object(private_access, 'invoke', lambda *a, **k: machines), \
                patch.object(winhost, 'job', lambda b, arguments: started.append(arguments) or fake), \
                patch.object(private_access, 'probe', lambda a: probed.append(json.loads(a.phase))):
            private_access.pair(args)
        argv = started[0]
        self.assertEqual(['exec', 'hp', '--timeout', '170s', '--', 'python3', '-c'], argv[:7])
        self.assertNotRegex(argv[7], r'[\n"]')  # one line that survives the Windows command line
        self.assertIn(b64.b64encode(private_access.PEER_ORIGIN.encode()).decode(), argv[7])
        self.assertEqual('private-address', argv[8])
        self.assertEqual({'state': 'private_access_required', 'check': 'private-address', 'target': 'hp', 'machine_id': 'machine_p',
                          'port': 40123, 'marker': argv[9]}, probed[0])
        self.assertEqual(['kill', 'close'], calls)
        with patch.dict(os.environ, {}, clear=True), self.assertRaises(SystemExit):
            private_access.pair(args)


if __name__ == '__main__':
    unittest.main()


@unittest.skipUnless(PWSH, 'set PB_TEST_PWSH to a pwsh binary to run the teammate sign-in for real')
class TeammateSigninOnTheDevice(unittest.TestCase):
    """teammate_signin.device_signin against a pb.exe stand-in, run through the real RUN snippet."""

    PB = ('#!/bin/sh\n'
          'case "$*" in\n'
          '"config show --json") echo \'{"data":{"server_url":"https://api.example.test","auth_file_fallback":true}}\';;\n'
          '*"auth status --json") if [ -f "$XDG_CONFIG_HOME/in" ]; then echo \'{"signed_in":true}\'; '
          'else echo \'{"signed_in":false}\'; exit 1; fi;;\n'
          '*"login --api-token-file -") mkdir -p "$XDG_CONFIG_HOME"; cat > "$XDG_CONFIG_HOME/in"; '
          'printf "%s|%s" "$2" "$XDG_STATE_HOME" > "$XDG_CONFIG_HOME/seen";;\n'
          '*) exit 9;;\nesac\n')

    def test_first_run_prepares_the_profile_and_logs_in_from_stdin_and_the_second_does_nothing(self):
        import tempfile
        import teammate_signin
        with tempfile.TemporaryDirectory() as home:
            exe = pathlib.Path(home) / 'pb.exe'
            exe.write_text(self.PB)
            exe.chmod(0o755)
            device = LocalDevice(pb_exe=str(exe), password='')
            device._environment = {'USERPROFILE': home}
            root = pathlib.Path(home) / '.pb-smoke' / 'teammate'
            minted = []
            with patch('winhost.device', lambda: device):
                self.assertTrue(teammate_signin.device_signin(lambda: minted.append(1) or b'pbt_secret'))
                self.assertFalse(teammate_signin.device_signin(lambda: self.fail('a signed-in profile mints no token')))
            self.assertEqual([1], minted)
            self.assertEqual({'server_url': 'https://api.example.test', 'auth': {
                'allow_file_fallback': True, 'profile_dir': str(root / 'credentials')}},
                json.loads((root / 'config.json').read_text()))
            self.assertEqual('pbt_secret', (root / 'config' / 'in').read_text())
            # the login ran with the profile's config and with the state directory moved off the device's registration
            self.assertEqual('%s|%s' % (root / 'config.json', root / 'state'), (root / 'config' / 'seen').read_text())

    def test_a_refused_token_is_an_error(self):
        import teammate_signin
        device = winhost.WindowsDevice(target='user@host', pb_exe=EXE, password='secret')
        device._environment = {'USERPROFILE': r'C:\Users\u'}
        fake = FakeSsh((0, b'yes'), (0, envelope(1, b'{"signed_in":false}')), (0, envelope(1, b'', b'pb: token refused')))
        with patch('winhost.device', lambda: device), patch('subprocess.run', fake):
            with self.assertRaisesRegex(RuntimeError, 'teammate sign-in failed: pb: token refused'):
                teammate_signin.device_signin(lambda: b'pbt_secret')
        login = fake.calls[2]
        self.assertEqual(r'--config C:\Users\u\.pb-smoke\teammate\config.json login --api-token-file -', login['payload']['args'])
        self.assertEqual(r'C:\Users\u\.pb-smoke\teammate\state', login['payload']['env']['XDG_STATE_HOME'])
        self.assertNotIn('pbt_secret', ' '.join(login['argv']))
