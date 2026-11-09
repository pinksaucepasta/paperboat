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
            self.assertEqual('secret', os.environ['SSHPASS'])
        self.assertEqual(['sshpass', '-e', 'ssh'], argv[:3])
        self.assertIn('-tt', argv)
        self.assertNotIn('secret', ' '.join(argv))
        command = argv[-1]
        self.assertIn("& 'C:\\Program Files\\Paperboat\\bin\\pb.exe' 'connect' 'my box' '--name' 'it''s'", command)

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


if __name__ == '__main__':
    unittest.main()
