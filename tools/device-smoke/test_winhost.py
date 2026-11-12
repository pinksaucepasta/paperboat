import base64
import json
import os
import pathlib
import subprocess
import sys
import tempfile
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
        opening, script, *chunks = input.decode().split('\n')
        assert opening == "$a=@'", opening
        payload = ''.join(chunks[:chunks.index("'@ -split \"`r?`n\"")])
        self.calls.append({'argv': argv, 'script': base64.b64decode(script).decode(),
                           'payload': json.loads(base64.b64decode(payload)), 'env': env})
        rc, out = self.replies.pop(0)
        return subprocess.CompletedProcess(argv, rc, out, b'')


class WindowsDeviceTest(unittest.TestCase):
    def device(self, password='secret'):
        return winhost.WindowsDevice(target='user@host', pb_exe=EXE, password=password)

    def test_a_read_survives_a_failed_ssh_session_and_a_change_does_not_repeat(self):
        device = self.device()
        calls = []
        replies = [(255, b'', b'Connection timed out during banner exchange'), (0, b'ok', b'')]
        def snippet(step, script, timeout=60, **payload):
            calls.append(script)
            return replies.pop(0) if replies else (0, b'ok', b'')
        device.snippet = snippet
        self.assertEqual((0, 'ok'), device.ps('read', winhost.READ))
        self.assertEqual(2, len(calls))
        replies[:] = [(255, b'', b'Connection timed out during banner exchange')]
        calls.clear()
        with self.assertRaises(winhost.DeviceError):
            device.ps('change', winhost.RUN)  # a snippet that changes the device is never sent twice
        self.assertEqual(1, len(calls))

    def test_command_line_reads_the_script_from_stdin_and_carries_no_secret(self):
        argv = self.device().ssh_command()
        self.assertEqual(['sshpass', '-e', 'ssh'], argv[:3])
        self.assertNotIn('secret', ' '.join(argv))
        self.assertTrue(argv[-1].endswith(' -Command -'), argv[-1])  # nothing but PowerShell reads the session's stdin
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

    def test_console_escapes_before_the_output_are_dropped(self):
        with patch('subprocess.run', FakeSsh((0, b'\x1b[?1h\x1b[?1l' + envelope(0, b'out', b'')))):
            self.assertEqual('out', self.device().run([EXE, 'x']).stdout)

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

    def test_a_read_only_snippet_is_sent_again_when_the_session_does_not_answer_but_a_write_is_not(self):
        calls = []

        def stalls_once(argv, input, capture_output, timeout, env):
            calls.append(timeout)
            if len(calls) == 1:
                raise subprocess.TimeoutExpired(argv, timeout)
            return subprocess.CompletedProcess(argv, 0, b'yes', b'')

        with patch('subprocess.run', stalls_once):
            self.assertTrue(self.device().exists(r'C:\x'))
        self.assertEqual([30, 60], calls)
        with patch('subprocess.run', side_effect=subprocess.TimeoutExpired('ssh', 60)) as run:
            with self.assertRaises(winhost.DeviceError):
                self.device().write_bytes(r'C:\x', b'data')
        self.assertEqual(1, run.call_count)

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
        return [PWSH, '-NoProfile', '-NonInteractive', '-Command', '-']


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
        import windows_checks
        defaults = dict(vars(windows_checks.build_parser().parse_args([])), machine='m', peer='p', team='t', owner_pb='pb')
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

    def test_protocol_gate_releases_the_gate_the_probe_result_names(self):
        import windows_checks
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            for code, gate in ((0, 'verified'), (1, 'failed')):
                probe = root / ('probe%d' % code)
                probe.write_text('#!/bin/sh\necho "$2 $4"\nexit %d\n' % code)
                probe.chmod(0o700)
                line = json.dumps({'state': 'preview_protocol_required', 'url': 'https://u.example.test', 'marker': 'M',
                                   'gate': str(root / 'verified'), 'failure_gate': str(root / 'failed')})
                self.assertEqual('https://u.example.test M\n', windows_checks.protocol_gate(probe)(line))
                self.assertEqual([gate], sorted(item.name for item in root.iterdir() if not item.name.startswith('probe')))
                (root / gate).unlink()
            self.assertEqual('', windows_checks.protocol_gate(probe)('{"state": "approval_required"}'))
            self.assertEqual([], [item.name for item in root.iterdir() if not item.name.startswith('probe')])

    def test_a_network_cut_is_scoped_timed_and_lifted_on_every_path(self):
        calls = []
        device = winhost.WindowsDevice(target='t', pb_exe='C:\\pb.exe')
        device.ps = lambda step, script, **kw: calls.append((script, kw.get('action'), kw.get('addr'), kw.get('inbound'))) or (0, '')
        device.start = lambda argv, directory: calls.append(('start', argv[0])) or 1
        device.temp_dir = lambda prefix: __import__('contextlib').nullcontext('dir')
        cut = device.cut_network(45)
        self.assertEqual(winhost.NET_CUT, calls[0][0])
        self.assertEqual(('add', winhost.NON_LOOPBACK, True), calls[0][1:])
        self.assertIn('-Program $x', winhost.NET_CUT)  # program scope: the controller's SSH is not pb.exe
        self.assertEqual(('start', 'powershell.exe'), calls[1])  # the timer that lifts it whatever happens here
        cut.release(), cut.release()
        self.assertEqual(['add', 'remove'], [c[1] for c in calls if c[0] == winhost.NET_CUT])
        calls.clear()
        device.start = lambda argv, directory: (_ for _ in ()).throw(winhost.DeviceError('no timer'))
        with self.assertRaises(winhost.DeviceError):
            device.cut_network(5, addresses=['203.0.113.7'], inbound=False)
        self.assertEqual(['add', 'remove'], [c[1] for c in calls if c[0] == winhost.NET_CUT])  # no timer: the rule goes again

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


class GateRelease(unittest.TestCase):
    def test_browser_gate_is_released_only_when_the_page_holds_the_marker(self):
        import http.server
        import tempfile
        import threading
        import windows_checks

        class Page(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200); self.end_headers(); self.wfile.write(b'<p>MARK-1</p>')

            def log_message(self, *args):
                pass

        server = http.server.HTTPServer(('127.0.0.1', 0), Page)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        try:
            with tempfile.TemporaryDirectory() as directory:
                base = {'state': 'browser_required', 'url': 'http://127.0.0.1:%d/' % server.server_port,
                        'gate': directory + '/ok', 'failure_gate': directory + '/bad'}
                windows_checks.browser_gate(json.dumps(dict(base, marker='MARK-1')))
                self.assertTrue(os.path.exists(directory + '/ok') and not os.path.exists(directory + '/bad'))
                os.unlink(directory + '/ok')
                with patch('time.monotonic', side_effect=[0, 0, 200]):
                    windows_checks.browser_gate(json.dumps(dict(base, marker='OTHER')))
                self.assertTrue(os.path.exists(directory + '/bad') and not os.path.exists(directory + '/ok'))
                windows_checks.browser_gate(json.dumps({'state': 'approval_required'}))  # not ours: nothing happens
        finally:
            server.shutdown()


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
    def test_windows_pid_reads_digits_with_or_without_a_newline(self):
        import update_continuity
        class Held:
            def __init__(self, *chunks):
                self.pending, self.chunks = bytearray(), list(chunks)
            def settle(self, quiet=0.5):
                if self.chunks:
                    self.pending.extend(self.chunks.pop(0))
        self.assertEqual('9368', update_continuity.windows_pid(Held(b'9368\r\n')))
        # ConPTY sometimes ends the line with cursor moves and the prompt; the digits arrive in two reads
        self.assertEqual('8456', update_continuity.windows_pid(Held(b'84', b'56\x1b]2;\x07\x1b[8;1HC:\\>')))

    def test_transfer_modes_target_script_reaches_the_target_as_one_word_for_ssh(self):
        import importlib
        import shlex
        env = {'TARGET_CMD': 'ssh host sudo -n docker exec -i box bash -c', 'PB_SMOKE_WINDOWS_PB': 'x'}
        with patch.dict(os.environ, env), patch.object(winhost, 'device', lambda: object()), \
                patch.object(winhost, 'pb_path', lambda: 'pb'):
            import transfer_modes_windows
            importlib.reload(transfer_modes_windows)
            script = 'sha256sum ~/Paperboat/Inbox/a.bin 2>/dev/null | cut -d" " -f1; echo $HOME'
            argv = transfer_modes_windows.target_argv(script)
        sys.modules.pop('transfer_modes_windows', None)
        self.assertEqual(script, shlex.split(' '.join(argv))[-1])  # what the remote shell hands to bash -c

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
        self.assertIn('--all', invoke.call_args.args)  # one page of an account with 56 machines hid the record
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
        self.assertEqual(3, len(calls))  # one piece, decode, remove
        self.assertEqual(['powershell', '-NoProfile'], list(calls[1][5:7]))
        calls.clear()
        with patch('exec_lifecycle.run', run), windows_target.staged('pb', 'victus', windows_target.exec_script('a', 'b' * 64, 'u', 'e', 1)):
            pass
        self.assertGreater(len(calls), 3)  # at least two pieces
        for call in calls:  # pb exec refuses a command of more than 4096 characters
            self.assertLess(sum(len(word) + 1 for word in call), 4000)

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
        with patch.dict(os.environ, {}, clear=True):
            started_in = winhost.terminal_argv(winhost.WindowsPb(device), ['new'], r"C:\Temp\pb-new-1'x")[-1]
        self.assertIn("Set-Location -LiteralPath 'C:\\Temp\\pb-new-1''x'; & 'C:\\Program Files\\Paperboat\\bin\\pb.exe' 'new'", started_in)

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

    def tools_run(self, windows, tools=None):
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
                    patch('sys.argv', ['x', 'peer'] + (['--tools', *tools] if tools else [])), \
                    patch('builtins.print', printed.append):
                ssh_tools_lifecycle.main()
            return calls, json.loads(printed[-1]), sorted(p.name for p in (home / 'Paperboat').iterdir())

    def test_tools_round_trip_to_a_unix_target_keeps_rsync_and_the_python_fixture(self):
        calls, receipt, left = self.tools_run(windows=False)
        self.assertEqual(['ssh', 'scp', 'scp', 'sftp', 'rsync', 'rsync', 'ssh'], [tool for tool, _ in calls])
        self.assertTrue(calls[0][1][2].startswith('python3 -c '))
        self.assertEqual(['scp', 'sftp', 'rsync'], receipt['tools'])
        self.assertEqual([], left)

    def test_tools_round_trip_to_a_windows_target_uses_powershell_and_home_relative_paths(self):
        for tools, expected in [(None, ['scp', 'sftp']), (['rsync'], ['rsync'])]:
            with self.subTest(tools=tools):
                calls, receipt, left = self.tools_run(windows=True, tools=tools)
                self.assertEqual([], left)
                transfers = ['scp', 'scp', 'sftp'] if tools is None else ['rsync', 'rsync']
                self.assertEqual(['ssh', *transfers, 'ssh'], [tool for tool, _ in calls])
                self.assertTrue(calls[0][1][2].startswith('powershell -NoProfile '))
                self.assertEqual('peer:Paperboat/' + self.fixture + ('/scp/' if tools is None else '/rsync/'), calls[1][1][2])
                self.assertEqual(expected, receipt['tools'])



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
            # reconnect_keeps_control no longer skips on a Windows target: the size is read from `pb session list --json`
            matrix.large_paste(SimpleNamespace(target='victus'), [])
            self.assertEqual(['SKIP'], [state for _, state in matrix.RESULTS])

    def test_device_clients_are_found_by_their_command_line_and_firewall_rows_use_the_device(self):
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
            self.assertTrue(matrix.has_iptables())  # the device's firewall is the cut (winhost.cut_network)
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


class McpOnTheDevice(unittest.TestCase):
    """mcp_exec.py: the source of mcp-smoke is pb.exe over ssh with one Windows command line, the target a container."""

    def test_source_runs_on_the_device_and_target_in_the_container(self):
        import mcp_exec
        device = winhost.WindowsDevice(target='winpool-9', pb_exe='C:\\P F\\pb.exe', password='')
        with patch('winhost.device', return_value=device):
            argv, _ = mcp_exec.command('lanesdk0-c', ['C:\\P F\\pb.exe', 'mcp', '--allow-send-dir', 'C:/t/allowed'])
        self.assertEqual(['winpool-9', '"C:\\P F\\pb.exe" mcp --allow-send-dir C:/t/allowed'], argv[-2:])
        self.assertIn('BatchMode=yes', argv)
        with patch.dict(os.environ, {'MCP_EXEC_HOST': 'host', 'MCP_EXEC_TARGET': 'ct'}):
            argv, _ = mcp_exec.command('lanesdk0-t', ['python3', '-c', "print('a b')"])
        self.assertEqual('host', argv[-2])
        self.assertTrue(argv[-1].endswith("XDG_RUNTIME_DIR=/run/user/1000 ct python3 -c 'print('\"'\"'a b'\"'\"')'"), argv[-1])


class FreePlanAndTagsOnTheDevice(unittest.TestCase):
    """free_plan.py reads the plan with an API token it always revokes; machine_tags.py routes `windows` to the device."""

    def test_the_plan_token_is_made_by_pb_on_the_device_and_revoked_even_when_the_check_fails(self):
        import free_plan
        import tempfile
        calls = []

        class Device:
            def runtime_dir(self):
                return pathlib.Path(self.directory)

        def pb_text(binary, arguments, **options):
            calls.append(list(arguments))
            return subprocess.CompletedProcess(arguments, 0, json.dumps({'data': {'secret': 'pbat_SECRET'}}), '')

        with tempfile.TemporaryDirectory() as directory:
            device = Device()
            device.directory = directory
            (pathlib.Path(directory) / 'machine-registration.json').write_text(json.dumps({'server_url': 'https://api.example.test'}))
            with patch('winhost.device', return_value=device), patch('winhost.pb_text', pb_text):
                with self.assertRaises(RuntimeError):
                    with free_plan.credential() as (token, server):
                        self.assertEqual(('pbat_SECRET', 'https://api.example.test'), (token, server))
                        raise RuntimeError('the check failed')
        name = calls[0][2]
        self.assertEqual(['api-token', 'create', name, '--workspace', 'personal', '--expires', '1d', '--json'], calls[0])
        self.assertEqual(['api-token', 'revoke', name, '--workspace', 'personal', '--yes'], calls[-1])

    def test_gate_events_are_counted_in_the_device_diagnostics(self):
        import free_plan
        asked = []

        class Device:
            path_flavor = pathlib.PureWindowsPath

            def env(self, name):
                return 'C:\\Users\\u\\AppData\\Local'

            def count_in_files(self, directory, pattern, text):
                asked.append((str(directory), pattern, text))
                return 1 if text == 'subscription_required' else 0

        with patch('winhost.device', return_value=Device()):
            self.assertEqual(1, free_plan.gate_events())
        self.assertEqual({'C:\\Users\\u\\AppData\\Local\\Paperboat\\state\\diagnostics'}, {a[0] for a in asked})
        self.assertIn('"http_status": "402"', [a[2] for a in asked])

    def test_machine_tags_runs_pb_on_the_device_for_the_windows_side(self):
        import machine_tags
        done = subprocess.CompletedProcess([], 0, 'out', 'err')
        with patch('winhost.side_run', return_value=done) as side_run:
            self.assertEqual((0, 'out', 'err'), machine_tags.sh('windows', '/unused/pb', ['machine', 'tag', 'x']))
        side_run.assert_called_once_with('windows', ['machine', 'tag', 'x'], timeout=150)


class ConfigSyncPairOnTheDevice(unittest.TestCase):
    def test_the_windows_side_gets_its_own_repository_endpoint_in_the_machine_file(self):
        import config_sync_pair
        self.assertIn('[pull]\nrepository_id = "r1"\nurl = "http://127.0.0.1:9/p.git"\n[push]\nrepository_id = "r1"\nurl = "http://127.0.0.1:9/p.git"\n',
                      config_sync_pair.machine_file('r1', url='http://127.0.0.1:9/p.git'))
        self.assertNotIn('url =', config_sync_pair.machine_file('r1'))

    def test_the_side_windows_is_the_device_and_anything_else_a_shell(self):
        import config_sync_pair
        device = winhost.WindowsDevice(target='winpool-9', pb_exe='C:\\\\pb.exe', password='')
        with patch('winhost.device', return_value=device), patch.object(winhost.WindowsDevice, 'env', return_value='C:\\\\Users\\\\u'):
            self.assertEqual('windows', config_sync_pair.machine('windows', 'w').os)
        self.assertEqual(['ssh', 'x'], config_sync_pair.machine('ssh x', 'u').prefix)


class FreePlanUnixPath(unittest.TestCase):
    """free_plan.main on a Unix device, with the account, pb and the diagnostics faked: every assertion still bites."""

    plan = {'state': 'free', 'seats_bought': 0, 'allowance_bytes': 5, 'usage_bytes': 1, 'allowance_period_end': '2026-12-01T00:00:00Z'}

    def run_main(self, plan=None, machines=None, exec_codes=(0,), diagnostics=''):
        import contextlib
        import io
        import tempfile
        import free_plan
        machines = machines if machines is not None else [{'alias': 't', 'state': 'online', 'permissions': ['terminal', 'exec']}]
        codes = list(exec_codes)

        def run(*arguments):
            if arguments[0] == 'machine':
                return 0, json.dumps({'machines': machines})
            return (codes.pop(0) if len(codes) > 1 else codes[0]), 'out'

        fake = SimpleNamespace(access_token=lambda: 'tok', server_url=lambda: 'https://api.example.test', DARWIN=False)
        with tempfile.TemporaryDirectory() as home:
            diagnostics_dir = pathlib.Path(home) / '.local/state/paperboat/diagnostics'
            diagnostics_dir.mkdir(parents=True)
            (diagnostics_dir / 'e.ndjson').write_text(diagnostics)
            out = io.StringIO()
            with patch('winhost.device', return_value=None), patch.dict(sys.modules, {'cli_credential': fake}), \
                    patch.object(free_plan, 'api', lambda path, token, server: plan or self.plan), patch.object(free_plan, 'run', run), \
                    patch.object(free_plan, 'HOME', pathlib.Path(home)), patch('time.sleep'), \
                    patch('sys.argv', ['free_plan.py', 't']), contextlib.redirect_stdout(out):
                free_plan.main()
        return json.loads(out.getvalue())

    def test_a_free_account_with_an_online_exec_machine_passes(self):
        self.assertEqual('PASS', self.run_main()['status'])

    def test_every_assertion_still_bites(self):
        for message, kwargs in (
                ('plain Free', dict(plan=dict(self.plan, state='team'))),
                ('plain Free', dict(plan=dict(self.plan, seats_bought=1))),
                ('allowance', dict(plan=dict(self.plan, usage_bytes=5))),
                ('online with terminal and exec', dict(machines=[])),
                ('online with terminal and exec', dict(machines=[{'alias': 't', 'state': 'online', 'permissions': ['terminal']}])),
                ('exec failed', dict(exec_codes=(1,))),
                ('plan gate', dict(diagnostics='{"code": "Subscription_Required"}\n')),
                ('plan gate', dict(diagnostics='{"http_status": "402"}\n'))):
            with self.subTest(message), self.assertRaisesRegex(RuntimeError, message):
                self.run_main(**kwargs)

    def test_exec_is_retried_before_it_fails(self):
        self.assertEqual('PASS', self.run_main(exec_codes=(1, 1, 0))['status'])


class ConfigCommandsRepositoryRows(unittest.TestCase):
    def test_every_row_is_selected_on_the_device_and_credentials_are_owner_only_by_acl(self):
        import config_commands
        device = SimpleNamespace(other_access=lambda path: ['S-1-1-0'] if str(path).endswith('open.json') else [])
        class P:
            def __init__(self, name):
                self.name = name
            def exists(self):
                return True
            def __str__(self):
                return self.name
        with patch('winhost.device', return_value=device):
            self.assertTrue(config_commands.owner_only(P('private.json')))
            self.assertFalse(config_commands.owner_only(P('open.json')))
        with patch('winhost.device', return_value=None), patch('config_commands.registration', return_value={'machine_id': 'm'}):
            self.assertEqual(config_commands.ROWS, config_commands.Suite(SimpleNamespace(rows=None)).selected)
        with patch('winhost.device', return_value=device), patch('config_commands.registration', return_value={'machine_id': 'm'}), \
                patch('winhost.pb_path', return_value='pb.exe'):
            self.assertIn('C169', config_commands.Suite(SimpleNamespace(rows=None)).selected)


class ExecCancelWatch(unittest.TestCase):
    def test_the_watch_reads_the_stream_pb_still_has_open_with_sharing(self):
        # a Windows device run (winpool-1) crashed here: [IO.File]::ReadLines opens without FileShare while pb.exe holds the file for writing
        import exec_cancel
        when = exec_cancel.nonce_published('abc123')
        self.assertNotIn('ReadLines', when)
        self.assertIn('[IO.FileShare]::ReadWrite', when)
        self.assertIn("-match 'abc123 [0-9]+'", when)


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


class FakeCmdMixin:
    def fake_cmd(self):
        import tempfile
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        script = pathlib.Path(directory.name) / 'cmd.py'
        script.write_text(FAKE_CMD)
        return [sys.executable, str(script), PWSH]


@unittest.skipUnless(PWSH, 'set PB_TEST_PWSH to a pwsh binary to run the ENV target scripts for real')
class EnvFamilyInPowerShell(FakeCmdMixin, unittest.TestCase):
    def run_ps(self, script, env):
        import windows_target
        argv = windows_target.ps_argv(script)
        argv[0] = PWSH
        return subprocess.run(argv, capture_output=True, env=dict(os.environ, **env))

    def test_printenv_and_bracket_scripts_print_what_the_sh_commands_print(self):
        import windows_target
        done = self.run_ps(windows_target.printenv_script('PB_T_SET'), {'PB_T_SET': 'p-1 λ'})
        self.assertEqual((0, 'p-1 λ\n'.encode()), (done.returncode, done.stdout))
        done = self.run_ps(windows_target.printenv_script('PB_T_UNSET'), {})
        self.assertEqual((1, b''), (done.returncode, done.stdout))
        self.assertEqual(b'[ok-value]\n', self.run_ps(windows_target.bracket_env_script('PB_T_SET'), {'PB_T_SET': 'ok-value'}).stdout)
        self.assertEqual(b'[]\n', self.run_ps(windows_target.bracket_env_script('PB_T_UNSET'), {}).stdout)
        with self.assertRaises(AssertionError):
            windows_target.printenv_script("X');exit 0;('")

    def test_diagnostics_scan_names_only_files_that_hold_a_value(self):
        import tempfile
        import windows_target
        with tempfile.TemporaryDirectory() as root:
            directory = pathlib.Path(root) / 'Paperboat' / 'state' / 'diagnostics' / 'nested'
            script = windows_target.diagnostics_scan_script(['p-secret', 'o-secret'])
            self.assertEqual(b'', self.run_ps(script, {'LOCALAPPDATA': root}).stdout)  # no directory yet
            directory.mkdir(parents=True)
            (directory / 'clean.ndjson').write_text('{"event":"p-other"}\n')
            self.assertEqual(b'', self.run_ps(script, {'LOCALAPPDATA': root}).stdout)
            (directory / 'leak.ndjson').write_text('{"event":"x","value":"o-secret"}\n')
            done = self.run_ps(script, {'LOCALAPPDATA': root})
            self.assertEqual((0, [str(directory / 'leak.ndjson')]), (done.returncode, done.stdout.decode().split()))

    def test_recursive_count_finds_a_value_below_the_directory(self):
        SnippetsInPowerShell.setUpClass()
        device = SnippetsInPowerShell.device
        with device.temp_dir('pb-test') as directory:
            (directory / 'deep' / 'events.ndjson').write_text('p-value p-value')
            self.assertEqual(0, device.count_in_files(directory, '*', 'p-value'))
            self.assertEqual(2, device.count_in_files(directory, '*', 'p-value', recurse=True))
            self.assertEqual(0, device.count_in_files(directory / 'missing', '*', 'p-value', recurse=True))

    def test_a_new_terminal_on_a_windows_target_reports_the_value(self):
        import env_scopes
        import windows_target
        argv = self.fake_cmd()
        with patch.object(winhost, 'terminal_argv', lambda binary, arguments: argv), \
                patch.object(env_scopes, 'windows', lambda machine: True), \
                patch.dict(os.environ, {'PB_T_TERM': 'o-77'}):
            self.assertEqual('o-77', env_scopes.terminal_sees('victus', 'PB_T_TERM', 'PBENVab12cd'))
            self.assertEqual('', env_scopes.terminal_sees('victus', 'PB_T_NONE', 'PBENVab12cd'))


class EnvFamilyPlumbing(unittest.TestCase):
    def test_pb_text_is_subprocess_run_here_and_run_remote_on_the_device(self):
        done = winhost.pb_text(sys.executable, ['-c', 'import sys;print(sys.stdin.read()+"!")'], input='in', timeout=30)
        self.assertEqual((0, 'in!\n', ''), (done.returncode, done.stdout, done.stderr))
        calls = []
        binary = SimpleNamespace(run_remote=lambda arguments, **options: calls.append((arguments, options)) or 'done')
        self.assertEqual('done', winhost.pb_text(binary, ('env', 'list'), input='x', timeout=9, env={'A': 'b'}))
        self.assertEqual([(('env', 'list'), {'input': 'x', 'env': {'A': 'b'}, 'timeout': 9})], calls)

    def test_sides_run_pb_on_the_device_as_its_account_or_its_second_profile(self):
        self.assertTrue(winhost.is_side('windows') and winhost.is_side('windows-teammate'))
        self.assertFalse(winhost.is_side('sudo docker exec ct pb'))
        with patch.dict(os.environ, {}, clear=True), self.assertRaisesRegex(RuntimeError, 'PB_SMOKE_WINDOWS_PB'):
            winhost.side_run('windows', ['env', 'list'])
        fake = FakeSsh((0, envelope(out=b'{}')), (0, rb'C:\Users\u'), (0, envelope(out=b'{}')))
        with patch.dict(os.environ, {'PB_SMOKE_WINDOWS_PB': EXE, 'VICTUS_PASS': 'p'}), patch('subprocess.run', fake):
            winhost._device.cache_clear()
            winhost.side_run('windows', ['env', 'list', '--json'], input='v')
            winhost.side_run('windows-teammate', ['RESET ENV TEAM t'])
        winhost._device.cache_clear()
        own, teammate = fake.calls[0]['payload'], fake.calls[2]['payload']
        self.assertEqual((EXE, 'env list --json', {}, 'dg=='), (own['exe'], own['args'], own['env'], own['input']))
        self.assertEqual(r'--config C:\Users\u\.pb-smoke\teammate\config.json "RESET ENV TEAM t"', teammate['args'])
        self.assertEqual(r'C:\Users\u\.pb-smoke\teammate\config', teammate['env']['XDG_CONFIG_HOME'])

    def test_env_scripts_route_pb_and_target_commands(self):
        import env_delivery
        import env_scopes
        import windows_target
        for name in ('env_scopes', 'env_messages', 'env_delivery', 'env_undeliverable', 'env_revoke_rotation'):
            source = (pathlib.Path(__file__).parent / (name + '.py')).read_text()
            self.assertNotIn("Path.home() / '.local/bin/pb'", source, name)
            self.assertNotIn('subprocess.run([str(PB)', source, name)
        seen = []
        done = subprocess.CompletedProcess([], 0, 'p-1\n', '')
        listing = subprocess.CompletedProcess([], 0, json.dumps({'machines': [
            {'id': 'machine_1', 'alias': 'victus', 'platform': 'windows'}, {'id': 'machine_2', 'alias': 'hp', 'platform': 'linux'}]}), '')
        answer = lambda binary, arguments, **options: seen.append(arguments) or (listing if arguments[0] == 'machine' else done)
        with patch.object(winhost, 'pb_text', answer), patch.dict(env_scopes.PLATFORMS, {}, clear=True):
            self.assertEqual('p-1', env_scopes.sees('hp', 'PB_N'))
            self.assertEqual(('exec', 'hp', '--', 'printenv', 'PB_N'), seen[-1])
            for machine in ('victus', 'machine_1'):  # by alias and by id
                env_scopes.sees(machine, 'PB_N')
                self.assertEqual(('exec', machine, '--', 'powershell'), seen[-1][:4])
            self.assertEqual(1, sum(arguments[0] == 'machine' for arguments in seen))  # the platforms are read once
        self.assertEqual(env_delivery.PB, env_scopes.PB)

    def test_leak_scan_on_the_device_counts_in_its_diagnostics_directory(self):
        import env_scopes
        import windows_target
        counted = []
        device = SimpleNamespace(env=lambda name: r'C:\Users\u\AppData\Local', path_flavor=pathlib.PureWindowsPath,
                                 count_in_files=lambda directory, pattern, text, recurse: counted.append((str(directory), text, recurse)) or (text == 'o-2'))
        with patch.object(winhost, 'device', lambda: device), patch.object(env_scopes, 'run', lambda *a, **k: (0, '', '')), \
                patch.object(env_scopes, 'windows', lambda machine: False):
            env_scopes.Output.seen = ['clean']
            self.assertEqual(['diagnostics on this machine'], env_scopes.leaked(('p-1', 'o-2'), 'hp'))
        self.assertEqual([(r'C:\Users\u\AppData\Local\Paperboat\state\diagnostics', 'p-1', True),
                          (r'C:\Users\u\AppData\Local\Paperboat\state\diagnostics', 'o-2', True)], counted)

    def test_team_scope_sides_and_runner_fixtures(self):
        import windows_checks
        table = windows_checks.checks(WindowsChecksRunner().args(vault_password_file=r'C:\v\pw'))
        self.assertEqual(['--teammate_vault_password_file'],
                         windows_checks.missing(table['env-team-scopes'][1], WindowsChecksRunner().args(vault_password_file='x')))
        self.assertEqual(['--vault_password_file'], windows_checks.missing(table['env-scopes'][1], WindowsChecksRunner().args()))
        argv = table['env-team-scopes'][0]
        self.assertEqual(('windows', 'windows-teammate'), (argv[argv.index('--owner-cmd') + 1], argv[argv.index('--member-cmd') + 1]))
        for check in ('env-messages', 'env-delivery', 'env-undeliverable', 'env-revoke-rotation'):
            self.assertTrue(windows_checks.missing(table[check][1], WindowsChecksRunner().args()), check)


class PolicyFamily(unittest.TestCase):
    """policy_cli.py against a scripted pb: the 13 steps of policy_cli.sh, their order and their cleanup."""

    def replies(self, **overrides):
        version = {'data': {'version': 7, 'can_edit': True}}
        table = {
            'auth status': (0, json.dumps({'account': {'email': 'me@example.test'}})),
            'policy schema': (0, json.dumps({'title': 'Paperboat workspace policy'})),
            'policy get --json': (0, json.dumps(version)),
            'policy get': (0, '{\n  // F18 device check: x\n  "tags": {"tag:smoke-ci": []}\n}'),
            'policy versions': (0, json.dumps({'data': {'versions': [{'kind': 'restored'}, {'kind': 'saved'}]}})),
            'policy test good': (0, 'ok'),
            'policy test bad': (1, 'error /rules/0/dst/0: unknown_machine'),
            'policy set': (0, 'saved'),
            'policy explain': (0, json.dumps({'decision': {'allowed': True, 'reason': 'owner'}})),
            'policy restore': (0, 'restored'),
            'api-token create': (0, json.dumps({'data': {'secret': 'pbat_SECRETVALUE', 'api_token': {'name': 'smoke-ci'}}})),
            'api-token list': (0, 'smoke-ci  exec'),
            'api-token revoke': (0, 'Revoked API token smoke-ci'),
        }
        table.update(overrides)
        return table

    def run_check(self, table):
        import contextlib
        import io
        import policy_cli
        calls = []

        def pb_text(binary, arguments, **options):
            key = ' '.join(arguments[:2])
            if key == 'policy get' and '--json' in arguments:
                key += ' --json'
            if key == 'policy test':
                key += ' bad' if arguments[-1].endswith('lkbad.json') else ' good'
                self.assertTrue(pathlib.Path(arguments[-1]).exists())  # the draft is there while pb reads it
            calls.append(key)
            code, out = table[key]
            return subprocess.CompletedProcess(arguments, code, out, '')

        printed = io.StringIO()
        with patch.object(winhost, 'pb_text', pb_text), patch.object(sys, 'argv', ['policy_cli.py', 'hp']), \
                contextlib.redirect_stdout(printed):
            code = policy_cli.main()
        return code, printed.getvalue(), calls

    def test_all_steps_pass_and_the_runner_reads_the_result(self):
        import windows_checks
        code, printed, calls = self.run_check(self.replies())
        self.assertEqual(0, code, printed)
        self.assertEqual(13, printed.count('PASS '))
        self.assertEqual('PASS', windows_checks.verdict(code, printed))
        self.assertEqual(['policy restore', 'policy versions', 'api-token create', 'api-token list', 'api-token revoke'], calls[-5:])
        self.assertIn('"dst": "machine:hp"', __import__('policy_cli').policy_file('me@example.test', 'hp'))

    def test_a_failed_step_still_restores_the_policy_revokes_the_token_and_hides_the_secret(self):
        import windows_checks
        code, printed, calls = self.run_check(self.replies(**{
            'policy explain': (1, 'denied'), 'api-token list': (0, 'smoke-ci pbat_SECRETVALUE')}))
        self.assertEqual(1, code)
        self.assertIn('RESULT FAIL C247-explain-owner,C252-list', printed)
        self.assertNotIn('SECRETVALUE', printed)
        self.assertIn('policy restore', calls)
        self.assertEqual('api-token revoke', calls[-1])
        self.assertEqual('FAIL', windows_checks.verdict(code, printed))
        code, printed, calls = self.run_check(self.replies(**{'policy set': (1, 'refused')}))
        self.assertNotIn('policy restore', calls)  # nothing was saved, so nothing is restored

    def test_policy_token_runs_the_windows_side_without_a_shell(self):
        import policy_token
        import windows_checks
        done = subprocess.CompletedProcess([], 3, 'out', 'err')
        with patch.object(winhost, 'side_run', return_value=done) as side:
            self.assertEqual((3, 'out', 'err'), policy_token.sh('windows', '/unused/pb', ('api-token', 'create', 'n'), stdin='s'))
        side.assert_called_once_with('windows', ('api-token', 'create', 'n'), input='s', timeout=150)
        with patch('subprocess.run', return_value=done) as local, patch.object(policy_token, 'timeout_words', lambda prefix: []):
            policy_token.sh('ssh host', '/home/pbt/.local/bin/pb', ('exec', 'a b'))
        self.assertEqual(['ssh', 'host'], local.call_args[0][0][:2])
        self.assertTrue(local.call_args[0][0][2].endswith("/home/pbt/.local/bin/pb exec 'a b'"))
        table = windows_checks.checks(WindowsChecksRunner().args())
        self.assertEqual(['--token_cmd', '--token_allowed', '--token_other'],
                         windows_checks.missing(table['policy-token'][1], WindowsChecksRunner().args()))
        self.assertEqual([], windows_checks.missing(table['policy-cli'][1], WindowsChecksRunner().args()))


class TeamFamily(unittest.TestCase):
    def business(self, **values):
        import team_business
        defaults = dict(cmd='windows', pb='/home/tester/.local/bin/pb', team='t', member='',
                        identity_commands=False, api='https://example.test', alias='', peer_cmd='',
                        streaming=False, s3_origin='')
        with patch.object(winhost, 'device', lambda: SimpleNamespace(env=lambda name: r'C:\Temp', path_flavor=pathlib.PureWindowsPath)):
            return team_business.Check(SimpleNamespace(**dict(defaults, **values)))

    def test_business_check_runs_only_pb_on_the_device_and_keeps_its_files_there(self):
        check = self.business()
        self.assertTrue(check.windows)
        files = {r'C:\Temp\f19.csv': b'id,action\n'}

        def read_bytes(path):
            if str(path) not in files:
                raise FileNotFoundError(path)
            return files[str(path)]

        device = SimpleNamespace(path_flavor=pathlib.PureWindowsPath, read_bytes=read_bytes, unlink=lambda path: files.pop(str(path), None))
        done = subprocess.CompletedProcess([], 0, '{"data": {"generation": 4}}', '')
        with patch.object(winhost, 'device', lambda: device), patch.object(winhost, 'side_run', return_value=done) as side:
            self.assertEqual(r'C:\Temp\f19.csv', check.file('f19.csv'))
            self.assertEqual({'generation': 4}, check.pbj('team', 'get', 't'))
            side.assert_called_once_with('windows', ['team', 'get', 't', '--json'], input=None, timeout=180)
            self.assertEqual((0, 'id,action\n', ''), check.read(check.file('f19.csv')))
            check.remove(check.file('f19.csv'))
            self.assertEqual(1, check.read(check.file('f19.csv'))[0])
        self.assertEqual({}, files)

    def test_business_token_helper_keeps_scope_and_secrets_private_on_the_controller(self):
        check = self.business()
        scope = json.dumps([{'dst': ['machine:peer'], 'allow': ['exec']}])
        done = subprocess.CompletedProcess([], 0, json.dumps({'data': {
            'secret': 'synthetic-token', 'api_token': {'id': 'token_1'}}}), '')
        try:
            with patch.object(winhost, 'side_run', return_value=done) as side:
                rc, out, err = check.run(['python3', '.f19helper.py', 'token', check.a.pb,
                                          'audit-scope', 'personal', '.f19stok', scope])
            self.assertEqual((0, 'token_1', ''), (rc, out, err))
            self.assertEqual(['api-token', 'create', 'audit-scope', '--expires', '1d',
                              '--workspace', 'personal', '--json', '--scope', scope], side.call_args.args[1])
            saved = pathlib.Path(check.helper_home.name) / '.f19stok'
            self.assertEqual('synthetic-token', saved.read_text())
            self.assertEqual(0o600, saved.stat().st_mode & 0o777)
            self.assertEqual((0, 'synthetic-token', ''), check.run(['cat', '.f19stok']))
            check.run(['rm', '-f', '.f19stok'])
            self.assertFalse(saved.exists())
        finally:
            if check.helper_home is not None:
                check.helper_home.cleanup()

    def test_business_check_on_unix_keeps_cat_rm_and_relative_names(self):
        import team_business
        check = team_business.Check(SimpleNamespace(cmd='ssh hp', pb='pb', team='t'))
        self.assertFalse(check.windows)
        self.assertEqual('f19.csv', check.file('f19.csv'))
        seen = []
        check.run = lambda args, **options: seen.append(args) or (0, '', '')
        check.read('f19.csv')
        check.remove('f19.csv', 'f19.json')
        self.assertEqual([['cat', 'f19.csv'], ['rm', '-f', 'f19.csv', 'f19.json']], seen)

    def test_business_stream_publishes_the_device_forward_and_keeps_it_until_cleanup(self):
        import contextlib
        import team_business
        check = self.business(streaming=True)
        events = []

        @contextlib.contextmanager
        def forward(port):
            events.append(('open', port))
            try:
                yield 18099
            finally:
                events.append(('closed', port))

        check.pbj = lambda *args: events.append(args) or {'endpoint': 'https://audit.example.test', 'id': 'tunnel_1'}
        with patch.object(winhost, 'device', lambda: SimpleNamespace(forward=forward)):
            self.assertEqual(('https://audit.example.test', 'tunnel_1'), team_business.tunnel(check, '8099'))
            self.assertEqual([('open', 8099), ('tunnel', '--ephemeral', '18099', '--background')], events)
            check.close()
        self.assertEqual(('closed', 8099), events[-1])

    def test_windows_main_runs_the_pb_only_sections(self):
        import contextlib
        import io
        import team_business
        ran = []
        check = self.business()
        names = ('roles', 'identity', 'identity_commands', 'search', 'export', 'machine_access', 'stream_https', 'stream_s3')
        with contextlib.ExitStack() as stack:
            for name in names:
                stack.enter_context(patch.object(team_business, name, lambda *a, name=name: ran.append(name) or 'member_1'))
            stack.enter_context(patch.object(check, 'pbj', lambda *a: {'owner_account': 'account_1'}))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            self.assertEqual(0, team_business.windows_main(check, SimpleNamespace(
                team='t', identity_commands=True, member='', alias='', peer_cmd='')))
        self.assertEqual(['roles', 'identity', 'identity_commands', 'search', 'export'], ran)

    def two_account(self, replies, *arguments):
        """Runs team_two_account.main() with both sides on the device; replies maps a command prefix to (rc, data)."""
        import contextlib
        import io
        import team_two_account
        calls = []

        def side_run(command, arguments, input=None, timeout=180):
            words = [word for word in arguments if word != '--json']
            calls.append((command, words))
            for prefix, reply in replies:
                if words[:len(prefix)] == prefix:
                    rc, value = reply(command, words) if callable(reply) else reply
                    return subprocess.CompletedProcess(arguments, rc, json.dumps(value), '')
            return subprocess.CompletedProcess(arguments, 0, '{}', '')

        printed = io.StringIO()
        argv = ['team_two_account.py', '--team', 't', '--owner-ssh', 'windows', '--machine', 'machine_1',
                '--member-cmd', 'windows-teammate', *arguments]
        with patch.object(winhost, 'side_run', side_run), patch.object(sys, 'argv', argv), contextlib.redirect_stdout(printed):
            code = team_two_account.main()
        return code, printed.getvalue(), calls

    def test_two_account_sides_are_the_device_account_and_its_second_profile(self):
        import windows_checks
        shared = []

        def machine_list(command, words):
            if command == 'windows':
                return 0, {'machines': [{'id': 'machine_1', 'ownership': 'personal'}]}
            return 0, {'machines': [{'id': 'machine_1', 'permissions': list(shared[-1])}] if shared and shared[-1] else []}

        def grant(command, words):
            active = '--active=false' not in words
            now = set(shared[-1]) if shared else set()
            wanted = set(words[words.index('--capability') + 1].split(','))
            shared.append(now | wanted if active else now - wanted)
            return 0, {}

        replies = [
            (['auth', 'status'], lambda command, words: (0, {'account': {'id': 'owner_1' if command == 'windows' else 'member_1'}})),
            (['team', 'get'], (0, {'generation': 3, 'members': [{'account_id': 'member_1', 'active': True}]})),
            (['team', 'machine', 'grant'], grant),
            (['--workspace', 't', 'machine', 'list'], machine_list),
            (['machine', 'list'], machine_list),
            (['team', 'activity'], lambda command, words: (0, {'items': [{'action': 'team.machine_' + kind} for kind in ('share', 'grant', 'unshare')]})
             if command == 'windows' else (1, {'error': {'code': 'forbidden'}})),
        ]
        code, printed, calls = self.two_account(replies, '--skip', 'approval,approval_rows,resources,suspension,transfer')
        self.assertEqual(0, code, printed)
        self.assertEqual({'windows', 'windows-teammate'}, {command for command, _ in calls})
        self.assertEqual(2, printed.count('PASS  '))
        self.assertEqual('PASS', windows_checks.verdict(code, printed))
        self.assertEqual('FAIL', windows_checks.verdict(1, printed.replace('PASS  F12-05', 'FAIL  F12-05')))

    def test_two_account_preview_origin_is_a_device_listener_that_is_always_stopped(self):
        events = []

        class Origin:
            def __enter__(self):
                events.append('start')
                return 41234

            def __exit__(self, *error):
                events.append('stop')

        replies = [
            (['auth', 'status'], lambda command, words: (0, {'account': {'id': command}})),
            (['team', 'get'], (0, {'generation': 3, 'members': [{'account_id': 'windows-teammate', 'active': True}]})),
            (['tunnel', 'create'], (1, {'error': {'code': 'refused'}})),
            (['preview', '41234'], lambda command, words: (events.append('preview') or 1, {'error': {'code': 'refused'}})),
        ]
        with patch.object(winhost, 'device', lambda: SimpleNamespace(origin=lambda body: Origin())):
            code, printed, calls = self.two_account(replies, '--skip', 'share,approval,approval_rows,suspension,transfer')
        self.assertEqual(1, code)
        self.assertEqual(['start', 'preview', 'stop'], events)
        self.assertIn('FAIL  F12-05 attach preview', printed)

    def test_runner_rows_and_fixtures(self):
        import windows_checks
        args = WindowsChecksRunner().args()
        table = windows_checks.checks(args)
        self.assertIn('approval,suspension,transfer', table['team-two-account'][0])
        self.assertEqual(['--suspend_cmd'], windows_checks.missing(table['team-suspension'][1], args))
        self.assertEqual(['--sso_team'], windows_checks.missing(table['team-identity-commands'][1], args))
        self.assertEqual([], windows_checks.missing(table['team-business'][1], args))
        self.assertNotIn('--identity-commands', table['team-business'][0])
        self.assertNotIn('--streaming', table['team-business'][0])
        configured = WindowsChecksRunner().args(
            f19_peer_cmd='ssh x', token_pb='/fixture/f19-peer/pb',
            f19_s3_origin='h:1', f19_s3_list_cmd='ls')
        row = windows_checks.checks(configured)['team-business'][0]
        for flag, value in {'--alias': 'm', '--peer-alias': 'p', '--peer-cmd': 'ssh x',
                            '--pb': '/fixture/f19-peer/pb', '--s3-origin': 'h:1',
                            '--s3-list-cmd': 'ls'}.items():
            self.assertIn(flag, row)
            self.assertEqual(value, row[row.index(flag) + 1])
        self.assertIn('--streaming', row)


@unittest.skipUnless(PWSH, 'set PB_TEST_PWSH to a pwsh binary to run the completion and listing snippets for real')
class RemainingFamilyInPowerShell(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        SnippetsInPowerShell.setUpClass()
        cls.device = SnippetsInPowerShell.device

    def test_files_lists_matching_names_and_a_missing_directory_is_empty(self):
        with self.device.temp_dir('pb-test') as directory:
            self.assertEqual([], self.device.files(directory / 'missing'))
            self.assertEqual([], self.device.files(directory))
            (directory / 'profile-1-access.secret').write_text('a')
            self.assertEqual(['profile-1-access.secret'], self.device.files(directory, '*.secret'))
            (directory / 'profile-1-refresh.secret').write_text('b')
            (directory / 'other.json').write_text('{}')
            (directory / 'nested' / 'deep.secret').write_text('c')
            self.assertEqual(['profile-1-access.secret', 'profile-1-refresh.secret'], sorted(self.device.files(directory, '*.secret')))
            self.assertEqual(3, len(self.device.files(directory)))

    def test_completions_load_the_script_from_its_text_with_pb_first_on_path(self):
        bin_directory = str(pathlib.PurePosixPath(self.device.pb_exe).parent)
        # Stands in for cobra's script: a completer for `pb` that answers only when pb's directory leads PATH.
        script = ("Register-ArgumentCompleter -Native -CommandName 'pb' -ScriptBlock {param($w,$ast,$c)\n"
                  "if(-not $env:PATH.StartsWith('%s')){return}\n"
                  "'exec','exit','doctor'|Where-Object{$_ -like \"$w*\"}|ForEach-Object{[Management.Automation.CompletionResult]::new($_+' ')}}\n"
                  % bin_directory)
        with self.device.temp_dir('pb-test') as directory:
            (directory / 'pb.ps1').write_text(script)
            self.assertEqual(['exec', 'exit'], self.device.completions(directory / 'pb.ps1', 'pb ex'))
            (directory / 'empty.ps1').write_text('$x=1\n')
            self.assertNotIn('exec', self.device.completions(directory / 'empty.ps1', 'pb ex'))
            (directory / 'broken.ps1').write_text('throw "bad script"\n')
            with self.assertRaises(winhost.DeviceError):
                self.device.completions(directory / 'broken.ps1', 'pb ex')

    def test_send_self_reads_the_inbox_and_cleans_up_on_the_device(self):
        import contextlib
        import io
        import send_self
        with self.device.temp_dir('pb-test') as inbox:
            record = {'machine_id': 'machine_1', 'inbox_path': str(inbox)}
            sent = []

            def invoke(binary, *arguments, **options):
                if arguments[-3:] == ('list', '--all', '--json'):  # one page of the account's machines hid the record
                    return {'machines': [{'id': 'machine_1', 'alias': 'victus'}]}
                source = winhost.RemotePath(self.device, arguments[1])
                target = inbox / source.name
                self.device.copy(source, target)  # the transfer: a local copy into the Inbox
                sent.append(target)
                return {'transfers': [{'state': 'delivered', 'receipt_path': str(target), 'transfer_id': 't1'}]}

            printed = io.StringIO()
            with patch.object(winhost, 'device', lambda: self.device), patch.object(send_self, 'invoke', invoke), \
                    patch.object(send_self, 'registration', lambda: record), patch.object(sys, 'argv', ['send_self.py', 'victus']), \
                    contextlib.redirect_stdout(printed):
                send_self.main()
            self.assertEqual('PASS', json.loads(printed.getvalue())['status'])
            self.assertEqual(1, len(sent))
            self.assertFalse(sent[0].exists())  # the delivered file was removed again
            self.assertEqual([], self.device.files(inbox))


class RemainingFamilyPlumbing(unittest.TestCase):
    PORTED = ('status_bar_lifecycle', 'completion_lifecycle', 'config_customize', 'workspace_lifecycle', 'auth_lifecycle',
              'bugreport_lifecycle', 'cli_paging', 'network_inventory', 'inventory_freshness', 'update_observation',
              'send_self', 'approve_refusals', 'session_limit', 'doctor_lifecycle')

    def test_ported_scripts_take_pb_from_winhost_and_the_runner_names_them(self):
        import windows_checks
        for name in self.PORTED + ('config_team_default',):
            source = (pathlib.Path(__file__).parent / (name + '.py')).read_text()
            self.assertNotIn(".local/bin/pb'", source, name)
            self.assertNotIn('subprocess.run([str(PB)', source, name)
            self.assertIn('winhost.pb_path()', source, name)
        args = WindowsChecksRunner().args()
        table = windows_checks.checks(args)
        scripts = {argv[0] for argv, _ in table.values()}
        for name in self.PORTED + ('config_team_default',):
            self.assertTrue(name + '.py' in scripts or name == 'auth_lifecycle', name)
        self.assertEqual('auth_lifecycle_approved.py', table['auth-lifecycle'][0][0])
        with patch.dict(os.environ, {}, clear=True):
            self.assertEqual(['STATE', 'PW_CHROMIUM'], windows_checks.missing(table['auth-lifecycle'][1], args))
        self.assertEqual(['--empty_team'], windows_checks.missing(table['workspace-switch'][1], args))
        self.assertEqual(['--config_repository'], windows_checks.missing(table['config-team-default'][1], args))
        for check in ('status-bar', 'completion', 'bugreport', 'inventory-freshness', 'session-limit', 'doctor', 'send-self',
                      'config-customize', 'cli-paging', 'network-inventory', 'update-observation', 'approve-refusals'):
            self.assertEqual([], windows_checks.missing(table[check][1], args), check)
        self.assertEqual('PASS', windows_checks.verdict(0, 'PASS F11-04 team-default-set - version 3\n{"x": {"ok": true}}\n'))
        self.assertEqual('FAIL', windows_checks.verdict(0, 'PASS a - b\nFAIL c - d\n'))

    def test_completion_snippet_does_not_stop_on_cobras_stderr_directive_line(self):
        self.assertLess(winhost.COMPLETE.index("$ErrorActionPreference='Continue'"), winhost.COMPLETE.index('TabExpansion2'))

    def test_interrupt_sends_break_from_a_detached_swallowing_caller_and_reaps_from_here(self):
        # spark/ctrlc-delivery.md: Ctrl-C never reached pb.exe (0/9), Ctrl-BREAK did (6/6); the BREAK needs a swallow
        # handler armed after AttachConsole; the answer is a file; a survivor is reaped by the controller.
        self.assertNotIn('[Console]::Out', winhost.INTERRUPT)
        self.assertNotIn('taskkill', winhost.INTERRUPT)
        self.assertNotIn("WindowStyle='Hidden'", winhost.INTERRUPT)
        self.assertLess(winhost.INTERRUPT.index('AttachConsole($p.Id)'), winhost.INTERRUPT.index('[Swallow]::Arm()'))
        self.assertIn('GenerateConsoleCtrlEvent(1,0)', winhost.INTERRUPT)
        self.assertIn('Set-Content -LiteralPath $req.result', winhost.INTERRUPT)
        import contextlib
        sent, stopped = {}, []

        class Result:
            def __init__(self, text):
                self.text = text

            def read_text(self):
                return self.text

        for text, want_stop in (('1 0.4 1 4242', False), ('1 90.0 -1 4242', True)):
            class Directory:
                def __truediv__(self, name):
                    return Result(text)

            @contextlib.contextmanager
            def temp_dir(prefix):
                yield Directory()

            stopped.clear()
            fake = SimpleNamespace(temp_dir=temp_dir, ps=lambda step, script, **payload: sent.update(payload), stop=stopped.append)
            import io
            said = io.StringIO()
            with contextlib.redirect_stderr(said):
                fired, code = winhost.WindowsDevice.interrupt_when(fake, [r'C:\pb.exe', 'setup'], 'x', seconds=150, exit_seconds=180)
            self.assertIn('interrupt via CTRL_BREAK', said.getvalue())
            self.assertEqual((True, int(text.split()[2])), (fired, code))
            self.assertEqual([4242] if want_stop else [], stopped)
        self.assertIn('result', sent)
        self.assertEqual(150, sent['seconds'])

    def test_enrollment_token_file_acl_is_system_and_the_user_only(self):
        # pb pair refuses a token file with one ACE (the user): token_file_windows.go wants SYSTEM and the user.
        self.assertIn('*S-1-5-18:(F)', winhost.TOKEN_FILE)
        self.assertIn('/inheritance:r', winhost.TOKEN_FILE)
        self.assertIn('/setowner', winhost.TOKEN_FILE)  # owner == the user, not Administrators
        self.assertIn('/remove "*S-1-5-32-544"', winhost.TOKEN_FILE)
        calls = []
        path = winhost.RemotePath(SimpleNamespace(path_flavor=pathlib.PureWindowsPath, ps=lambda step, script, **payload: calls.append((script, payload))), r'C:\Temp\token')
        path.enrollment_token_file()
        self.assertEqual([(winhost.TOKEN_FILE, {'path': r'C:\Temp\token'})], calls)

    def test_enrollment_file_takes_the_windows_token_for_a_device(self):
        # Tokens are per OS: the Linux one is refused by pb pair on Windows (hostruntimecmd/main.go, bootstrap.go).
        import pairing_lifecycle
        receipt = {'windows_command': "powershell -c \"iex (irm 'https://x.test/install.ps1?p=name-0123456789ABCDEFGHIJKLMNOP')\"",
                   'linux_macos_command': "curl 'https://x.test/i?p=ZZZZZZZZZZZZZZZZZZZZZZZZZZ'"}
        written = []

        class File:
            def write_text(self, text):
                written.append(text)

            def enrollment_token_file(self):
                written.append('acl')

        class Directory:
            def __truediv__(self, name):
                return File()

        with patch.object(pairing_lifecycle, 'invoke', return_value=receipt):
            pairing_lifecycle.enrollment_file(SimpleNamespace(device=object()), 'alias', Directory())
        self.assertEqual(['0123456789ABCDEFGHIJKLMNOP', 'acl'], written)

    def test_a_windows_uninstall_is_awaited_until_its_helper_status_reads_completed(self):
        import pairing_lifecycle
        states = iter(['pending', 'removing', 'completed'])

        class Status:
            def read_text(self):
                return json.dumps({'state': next(states)})

        with patch.object(winhost, 'device', lambda: object()), patch.object(winhost, 'device_path', lambda path: Status()), \
                patch.object(pairing_lifecycle.time, 'sleep', lambda seconds: None):
            pairing_lifecycle.await_windows_uninstall({'data': {'completion_status_path': r'C:\\Temp\\status.json'}})
            self.assertEqual([], list(states))  # read three times, no more
            states = iter(['failed'])
            with self.assertRaisesRegex(RuntimeError, 'ended failed'):
                pairing_lifecycle.await_windows_uninstall({'completion_status_path': r'C:\\Temp\\status.json'})
        pairing_lifecycle.await_windows_uninstall({'data': {}})  # no status path (Unix): nothing to wait for

    def test_install_interrupt_watches_the_elevation_bridge_helper_not_a_temp_copy(self):
        import install_lifecycle
        self.assertIn("helper.exe", install_lifecycle.INSTALLER_RUNNING)
        self.assertIn("__runtime-service", install_lifecycle.INSTALLER_RUNNING)
        self.assertNotIn("pb-elevated", install_lifecycle.INSTALLER_RUNNING)
        source = (pathlib.Path(__file__).parent / 'pairing_lifecycle.py').read_text()
        self.assertNotIn("'list', '--json')['machines']", source)  # the default machine page is 50 of 74

    def test_doctor_gate_accepts_only_the_http_sys_busy_port_warning_on_a_windows_device(self):
        import contextlib
        import io
        import pairing_lifecycle as p
        owner = 'Windows HTTP service (HTTP.sys, for example IIS), PID 4'

        def report(owner_text, extra=()):  # the real shape of `pb doctor --json` with the busy-port warning
            busy = 'Browser URLs are off because another program is using port 80/443 (%s)' % owner_text
            return {'overall': 'degraded', 'checks': [
                {'category': 'control', 'code': 'authentication', 'status': 'pass', 'summary': 'ok'},
                {'category': 'local', 'code': 'daemon', 'status': 'warning', 'summary': 'The local daemon is ready. ' + busy + '.'},
                {'category': 'daemon', 'code': 'daemon:browser_urls_unavailable', 'status': 'warning', 'summary': busy}, *extra]}

        class Doctor:  # exits 1 like a degraded `pb doctor --json`; its report is still on stdout
            def run_remote(self, arguments, **_):
                return subprocess.CompletedProcess(arguments, 1, json.dumps(report(owner)), '')
        with patch.object(winhost, 'device', lambda: object()), contextlib.redirect_stdout(io.StringIO()):
            self.assertTrue(p.doctor_healthy(p.doctor_report(Doctor())))
        failing = {'category': 'local', 'code': 'peer_paths', 'status': 'fail', 'summary': 'no path'}
        self.assertTrue(p.doctor_healthy({'overall': 'healthy', 'checks': []}))
        with patch.object(winhost, 'device', lambda: object()):
            printed = io.StringIO()
            with contextlib.redirect_stdout(printed):
                self.assertTrue(p.doctor_healthy(report(owner)))
            self.assertIn('tolerated the busy-port warning', printed.getvalue())
            self.assertIn('HTTP.sys PID 4', printed.getvalue())
            self.assertFalse(p.doctor_healthy(report('nginx, PID 4242')))  # any other owner fails the gate
            self.assertFalse(p.doctor_healthy(report('unknown program, PID 4')))
            self.assertFalse(p.doctor_healthy(report(owner, [failing])))
            self.assertFalse(p.doctor_healthy(report(owner, [{'code': 'clock', 'status': 'warning', 'summary': 'skewed'}])))
            self.assertFalse(p.doctor_healthy({'overall': 'unhealthy', 'checks': []}))
        with patch.object(winhost, 'device', lambda: None):  # Linux/macOS stay strict
            self.assertFalse(p.doctor_healthy(report(owner)))

    def test_wait_windows_shell_resends_a_line_the_attach_swallowed(self):
        import terminal_lifecycle

        class Swallowing:
            """The first typed line is swallowed (sent before pb connect attached); the retry answers."""
            def __init__(self):
                self.sent, self.attempts = [], 0

            def send(self, data):
                self.sent.append(data)

            def expect(self, token, timeout=45):
                self.attempts += 1
                if self.attempts == 1:
                    raise RuntimeError('terminal did not produce the expected fixture output')
                return token

        held = Swallowing()
        terminal_lifecycle.wait_windows_shell(held, timeout=60)
        self.assertEqual((2, 2), (held.attempts, len(held.sent)))
        self.assertNotEqual(held.sent[0], held.sent[1])  # a fresh marker each time: a late echo of the lost line cannot answer the retry

        class Silent(Swallowing):
            def expect(self, token, timeout=45):
                raise RuntimeError('terminal did not produce the expected fixture output')
        with self.assertRaises(RuntimeError):
            terminal_lifecycle.wait_windows_shell(Silent(), timeout=1)

    def test_edge_restart_previews_imports_its_origin_from_preview_lifecycle(self):
        import edge_restart_previews  # the module's imports resolve (serve() left preview_survival in 91440c787)
        self.assertTrue(callable(edge_restart_previews.stoppable_origin))

    def test_edge_restart_pin_requires_every_preview_to_serve_from_the_edge_ip(self):
        import edge_restart_previews as erp
        with tempfile.TemporaryDirectory() as directory:
            done = pathlib.Path(directory, 'done')
            done.write_text('')
            seen = []

            def fetch(endpoint, ip):
                seen.append((endpoint, ip))
                return 'M' if endpoint != 'https://b.example.test' else ''
            with patch.object(erp, 'pinned_body', fetch), patch.object(erp.time, 'sleep', lambda s: None):
                served, failed = erp.pin_every_preview(['https://a.example.test', 'https://b.example.test'], 'M', '192.0.2.1', done, serve_s=0)
            self.assertEqual(([0], [1]), (list(served), failed))
            self.assertTrue(all(ip == '192.0.2.1' for _, ip in seen))
            # the restart never finishes: nothing is claimed served
            served, failed = erp.pin_every_preview(['https://a.example.test'], 'M', '192.0.2.1', pathlib.Path(directory, 'never'), wait_s=0)
            self.assertEqual(({}, [0]), (served, failed))

    def test_pb_text_keeps_the_stdin_of_the_local_call(self):
        done = winhost.pb_text(sys.executable, ['-c', 'import sys;print(len(sys.stdin.read()))'], timeout=30, stdin=subprocess.DEVNULL)
        self.assertEqual('0\n', done.stdout)
        remote = SimpleNamespace(run_remote=lambda arguments, **options: options)
        self.assertEqual({'input': None, 'env': None, 'timeout': 30}, winhost.pb_text(remote, ['x'], timeout=30, stdin=subprocess.DEVNULL))

    def completion(self, device):
        import contextlib
        import io
        import completion_lifecycle
        replies = {'completion': 'pb ' + 'x' * 600, '__complete': 'exec\ndoctor\nmachine\nhp\n'}
        printed = io.StringIO()
        with patch.object(winhost, 'device', lambda: device), patch.object(sys, 'argv', ['completion_lifecycle.py', 'hp']), \
                patch.object(completion_lifecycle, 'run', lambda *arguments, **options: (0, replies[arguments[0]])), \
                contextlib.redirect_stdout(printed):
            completion_lifecycle.main()
        return json.loads(printed.getvalue())

    def test_completion_checks_powershell_on_the_device_and_bash_here(self):
        class Directory:
            def __enter__(self):
                return winhost.RemotePath(device, r'C:\Temp\pb-completion-1')

            def __exit__(self, *error):
                pass

        loaded = []
        device = SimpleNamespace(path_flavor=pathlib.PureWindowsPath, temp_dir=lambda prefix: Directory(),
                                 write_bytes=lambda path, data: loaded.append(str(path)),
                                 completions=lambda script, line: ['exec', 'exit'] if line == 'pb ex' else [])
        receipt = self.completion(device)
        self.assertTrue(receipt['powershell_registered'])
        self.assertNotIn('bash_registered', receipt)
        self.assertEqual([r'C:\Temp\pb-completion-1\pb.ps1'], loaded)
        device.completions = lambda script, line: []
        with self.assertRaisesRegex(RuntimeError, 'PowerShell did not register'):
            self.completion(device)
        import completion_lifecycle
        with patch.object(completion_lifecycle, 'unix_shells_accept', lambda scripts: None):
            receipt = self.completion(None)
        self.assertEqual(['bash_registered', 'check', 'commands_complete', 'machine', 'machine_names_complete', 'scripts', 'status'],
                         sorted(receipt))

    def test_device_login_writes_the_link_waits_and_always_ends_the_job(self):
        import tempfile
        import auth_lifecycle

        class Directory:
            def __enter__(self):
                return 'dir'

            def __exit__(self, *error):
                pass

        def job(stdout, code):
            return SimpleNamespace(stdout=lambda: stdout, stderr=lambda: b'', returncode=lambda: None, killed=[],
                                   wait=lambda timeout: code, kill=lambda: jobs[-1].killed.append(True))

        jobs = []
        device = SimpleNamespace(temp_dir=lambda prefix: Directory(), spawn=lambda argv, directory: jobs[-1])
        link = json.dumps({'data': {'state': 'approval_required', 'approval_url': 'https://console.example.test/approve'}}).encode() + b'\n'
        with tempfile.TemporaryDirectory() as directory, patch.object(auth_lifecycle, 'PB', SimpleNamespace(device=device)):
            url = pathlib.Path(directory) / 'url'
            jobs.append(job(link, 0))
            auth_lifecycle.device_login(url)
            self.assertEqual('https://console.example.test/approve\n', url.read_text())
            self.assertEqual([True], jobs[-1].killed)
            jobs.append(job(link, 3))
            with self.assertRaisesRegex(RuntimeError, 'login exited 3'):
                auth_lifecycle.device_login(url)
            self.assertEqual([True], jobs[-1].killed)
            jobs.append(job(b'{"data": {"state": "signed_in"}}\n', 0))
            with self.assertRaisesRegex(RuntimeError, 'approval link'):
                auth_lifecycle.device_login(url)

    def test_inventory_alias_probe_uses_the_device_ssh_client(self):
        import inventory_freshness
        calls = []

        def run_remote(arguments, timeout):
            calls.append(arguments)
            if 'slow.example.test' in arguments:
                raise subprocess.TimeoutExpired(arguments, timeout)
            return subprocess.CompletedProcess(arguments, 0, '', '')

        with patch.object(winhost, 'device', lambda: object()), patch.object(winhost, 'which', lambda name: SimpleNamespace(run_remote=run_remote)):
            self.assertTrue(inventory_freshness.resolves('hp.example.test'))
            self.assertFalse(inventory_freshness.resolves('slow.example.test'))
        self.assertEqual(['-o', 'BatchMode=yes', '-o', 'ConnectTimeout=5', 'hp.example.test', 'true'], calls[0])
        done = subprocess.CompletedProcess([], 0, b'', b'')
        with patch.object(winhost, 'device', lambda: None), patch('subprocess.run', return_value=done) as local:
            self.assertTrue(inventory_freshness.resolves('hp.example.test'))
        self.assertEqual(['ssh', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=5', 'hp.example.test', 'true'], local.call_args[0][0])

    def test_terminal_checks_type_a_powershell_marker_at_a_windows_target(self):
        import doctor_lifecycle
        import session_limit
        import windows_target

        class Fake:
            def __init__(self, arguments, env=None):
                self.sent, self.expected, self.output = [], [], bytearray()

            def send(self, data):
                self.sent.append(data)

            def expect(self, token, timeout=45):
                self.expected.append(token)

            def marker(self, label, prefix=''):
                return 'PB_UNIX_MARKER'

            def wait_exit(self, timeout=15):
                return 0

            def close(self):
                made.append(self)

        made = []
        for windows in (True, False):
            with patch.object(session_limit, 'Terminal', Fake), patch.object(windows_target, 'is_windows', lambda binary, target: windows):
                self.assertEqual((0, ''), session_limit.new('smoke-limit-1', 'target'))
            with patch('terminal_lifecycle.Terminal', Fake), patch('session_cleanup.cleanup', lambda *a: None), \
                    patch.object(windows_target, 'is_windows', lambda binary, target: windows):
                self.assertTrue(doctor_lifecycle.terminal_works('target'))
        limit_windows, doctor_windows, limit_unix, doctor_unix = made
        self.assertEqual([b'\x1b[?2004h', 'PB_UNIX_MARKER'], limit_unix.expected)
        self.assertEqual([b'\x04'], doctor_unix.sent)
        self.assertEqual(1, len(limit_windows.expected))
        self.assertTrue(limit_windows.sent[0].startswith('powershell -NoProfile -Command'))
        self.assertNotIn(limit_windows.expected[0], limit_windows.sent[0])  # the echo of the typed line is not the marker
        self.assertEqual('exit\r', doctor_windows.sent[-1])

    def test_bugreport_reads_stored_secret_files_from_the_device(self):
        import bugreport_lifecycle
        files = {'profile-0123456789abcdef-access.secret': b'\xff\xfeopaque-dpapi-blob-of-the-device', 'machine-public.secret': b'{"k": "v"}'}
        device = SimpleNamespace(path_flavor=pathlib.PureWindowsPath, files=lambda directory, pattern: sorted(files),
                                 read_bytes=lambda path: files[path.name], cli_secrets=lambda: {'0123456789abcdef': 'dpapi-secret-value-of-the-device'})
        config = winhost.RemotePath(device, r'C:\Users\u\AppData\Local\paperboat')
        with patch.object(bugreport_lifecycle, 'DEVICE', device), patch.object(bugreport_lifecycle, 'CONFIG', config), \
                patch.object(bugreport_lifecycle, 'RUNTIME', config / 'missing'), patch.object(bugreport_lifecycle, 'DARWIN', False):
            device.read_bytes = lambda path: files[path.name] if path.name in files else (_ for _ in ()).throw(FileNotFoundError(path))
            found = bugreport_lifecycle.secrets()
        self.assertIn('profile-*-access.secret', found.values())
        self.assertIn('env-value', found.values())
        self.assertIn('dpapi-01234567', found.values())
        self.assertEqual(3, len(found))



# Stands in for `ssh [options] HOST <command>`: the command's `powershell` is PB_TEST_PWSH on this machine.
FAKE_SSH = r"""
import os, shlex, sys
words = shlex.split(sys.argv[-1])
assert words[0] == 'powershell' and sys.argv[-2] == 'device', sys.argv
os.execv(sys.argv[1], [sys.argv[1]] + words[1:])
"""


@unittest.skipUnless(PWSH, 'set PB_TEST_PWSH to a pwsh binary to run the ENV hook shells for real')
class EnvHookInPowerShell(unittest.TestCase):
    """env_ssh_hook's Windows shells against a real PowerShell whose profile stands in for the installed hook."""

    def setUp(self):
        import tempfile
        self.home = tempfile.TemporaryDirectory()
        self.addCleanup(self.home.cleanup)
        ssh = pathlib.Path(self.home.name) / 'ssh.py'
        ssh.write_text(FAKE_SSH)
        self.profile = pathlib.Path(self.home.name) / 'powershell' / 'profile.ps1'  # pwsh on Linux: $XDG_CONFIG_HOME/powershell
        self.profile.parent.mkdir()
        self.profile.write_text("$env:PB_ENV_HOOK_PROBE='from-profile'\nfunction pb { if ($args.Count -eq 2 -and "
                                "$args[1] -eq 'load') { $env:PB_ENV_HOOK_PROBE='from-load' } }\n")
        patcher = patch.dict(os.environ, {'XDG_CONFIG_HOME': self.home.name})
        patcher.start()
        self.addCleanup(patcher.stop)
        os.environ.pop('PB_ENV_HOOK_PROBE', None)
        self.device = SimpleNamespace(ssh_prefix=lambda *options: [sys.executable, str(ssh), PWSH, *options],
                                      ssh_environment=lambda: None, password='')

    def sha(self, value):
        import hashlib
        return hashlib.sha256(value.encode()).hexdigest()

    def test_a_plain_ssh_command_and_an_interactive_shell_load_the_profile_and_show_only_the_hash(self):
        import env_ssh_hook
        self.assertNotIn('PROBE ', env_ssh_hook.PS_SHOW)  # a console echoes the typed line
        done = env_ssh_hook.windows_shell(self.device, 'device', env_ssh_hook.PS_SHOW)
        self.assertEqual((0, self.sha('from-profile')), (done.returncode, env_ssh_hook.probe_hash(done.stdout)))
        self.assertNotIn('from-profile', done.stdout + done.stderr)
        shown = env_ssh_hook.windows_typed(self.device, 'device', env_ssh_hook.PS_SHOW)
        self.assertEqual(self.sha('from-profile'), env_ssh_hook.probe_hash(shown))
        self.assertNotIn('from-profile', shown)
        loaded = env_ssh_hook.windows_typed(
            self.device, 'device', 'Remove-Item Env:PB_ENV_HOOK_PROBE; pb env load *>$null; ' + env_ssh_hook.PS_SHOW)
        self.assertEqual(self.sha('from-load'), env_ssh_hook.probe_hash(loaded))

    def test_a_shell_without_the_hook_shows_the_hash_of_nothing(self):
        import env_ssh_hook
        self.profile.unlink()
        done = env_ssh_hook.windows_shell(self.device, 'device', env_ssh_hook.PS_SHOW)
        self.assertEqual(self.sha(''), env_ssh_hook.probe_hash(done.stdout))
        self.assertIsNone(env_ssh_hook.probe_hash("typed: WriteLine('PRO'+'BE '+$h)"))

    def test_emit_timing_counts_stdout_and_stderr_of_the_quiet_paths_on_the_device(self):
        import env_ssh_hook
        SnippetsInPowerShell.setUpClass()
        device = SnippetsInPowerShell.device
        fake = pathlib.Path(self.home.name) / 'pb.exe'
        fake.write_text('#!/bin/sh\n[ "$*" = "env hook --emit powershell" ] || exit 9\n'
                        '[ -n "$PAPERBOAT_RUNTIME_STATE_ROOT" ] && exit 0\n'
                        '[ "$PAPERBOAT_CONFIG" = "$HOME/missing/config.json" ] && { echo notice >&2; exit 0; }\n'
                        'sleep 0.2; echo "\\$env:A = \'v\'"\n')
        fake.chmod(0o755)
        with patch.dict(os.environ, {'HOME': self.home.name}):
            out = device.ps('time the hook', env_ssh_hook.EMIT_TIMING, exe=str(fake), missing=self.home.name + '/missing')[1]
        cached, no_runtime, no_login, quiet_ms = map(int, out.split())
        self.assertEqual((0, len('notice\n')), (no_runtime, no_login))
        self.assertTrue(200 <= cached < 2000 and quiet_ms < 2000, out)


class ScriptedHookDevice:
    """pb.exe and the two profile files of a Windows device, as far as the ENV hook checks use them."""

    MARKED = b"if (Test-Path -LiteralPath 'C:\\pb.exe') { } # paperboat-env-hook\n"

    def __init__(self, test, uninstall_keeps=b''):
        self.test, self.value, self.names_on, self.calls, self.uninstall_keeps = test, None, True, [], uninstall_keeps
        self.was_set = None
        self.disk = {r'C:\Users\u\Documents\WindowsPowerShell\profile.ps1': b'Set-Alias ll dir\r\n'}  # the other is missing
        self.before = dict(self.disk)
        self.target, self.pb_exe, self.password, self.path_flavor = 'device', EXE, '', pathlib.PureWindowsPath

    def read_bytes(self, path):
        if str(path) not in self.disk:
            raise FileNotFoundError(path)
        return self.disk[str(path)]

    def env(self, name):
        return {'TEMP': r'C:\Temp', 'USERPROFILE': r'C:\Users\u'}[name]

    def ps(self, step, script, **payload):
        import env_ssh_hook
        if script == env_ssh_hook.DOCUMENTS:
            return 0, r'C:\Users\u\Documents'
        self.test.assertEqual(env_ssh_hook.EMIT_TIMING, script)
        self.test.assertTrue(payload['missing'].startswith('C:\\Temp\\pb-smoke-missing-'))
        return 0, '41 0 0 120'

    def sha(self):
        import hashlib
        return 'PROBE %s\n' % hashlib.sha256((self.value or '').encode()).hexdigest()

    def pb(self, *arguments, stdin=None):
        self.calls.append(arguments)
        done = lambda rc=0, out='', err='': subprocess.CompletedProcess(arguments, rc, out, err)
        if arguments[:2] == ('env', 'set'):
            self.value = self.was_set = stdin
        elif arguments[:2] == ('env', 'unset'):
            if '--confirm' in arguments:
                self.value = None
            return done(1, json.dumps({'data': {'confirmation_token': 'c-1'}}))
        elif arguments == ('env', 'hook', 'powershell', '--install'):
            for name in ('WindowsPowerShell', 'PowerShell'):
                path = r'C:\Users\u\Documents\%s\profile.ps1' % name
                kept = self.disk.get(path, b'').replace(self.MARKED, b'')
                self.disk[path] = (kept or b'\xef\xbb\xbf') + self.MARKED
        elif arguments == ('env', 'hook', '--uninstall'):
            self.disk = {path: data.replace(b'\xef\xbb\xbf' + self.MARKED, b'').replace(self.MARKED, self.uninstall_keeps)
                          for path, data in self.disk.items()}
        elif arguments[:3] == ('env', 'names', 'access'):
            self.names_on = arguments[3] == 'on'
        elif arguments[:2] == ('env', 'names'):
            if not self.names_on:
                return done(1, '', 'Listing ENV names is turned off for this workspace.\n')
            if '--json' in arguments:
                return done(0, json.dumps({'data': {'names': [{'name': 'PB_ENV_HOOK_PROBE', 'scope': 'personal'}]}}))
            return done(0, 'PB_ENV_HOOK_PROBE\n')
        elif arguments[:3] == ('env', 'run', '--'):
            if arguments[3] == 'cmd':
                return done(7)
            self.test.assertEqual(('powershell', '-NoProfile'), arguments[3:5])  # the child cannot get it from the hook
            return done(0, self.sha())
        return done()

    def hooked(self):
        return any(self.MARKED in data for data in self.disk.values())

    def shell(self, device, host, script, timeout=120):
        self.test.assertEqual('device', host)
        return subprocess.CompletedProcess([], 0, self.sha() if self.hooked() else 'PROBE %s\n' % ('0' * 64), '')

    def typed(self, device, host, line):
        return 'PS C:\\> typed\r\n' + self.shell(device, host, line).stdout

    def patches(self, module):
        import env_ssh_hook
        return [patch.object(winhost, 'device', lambda: self), patch.object(module, 'windows_pb', self.pb),
                patch.object(env_ssh_hook, 'windows_pb', self.pb), patch.object(module, 'windows_shell', self.shell),
                patch.object(env_ssh_hook, 'windows_typed', self.typed)]


def run_hook_check(module, device, argv):
    """module.main() against the scripted device: (exit code, report), or (the check's own error, {})."""
    import contextlib
    import io
    out = io.StringIO()
    with contextlib.ExitStack() as stack:
        for item in device.patches(module):
            stack.enter_context(item)
        stack.enter_context(patch.object(sys, 'argv', argv))
        stack.enter_context(contextlib.redirect_stdout(out))
        try:
            code = module.main()
        except Exception as error:
            return error, {}
    return code, json.loads(out.getvalue())


class EnvHookPlumbing(unittest.TestCase):
    def test_env_ssh_hook_on_the_device_runs_every_step_and_restores_both_profiles(self):
        import env_ssh_hook
        import windows_checks
        device = ScriptedHookDevice(self)
        code, report = run_hook_check(env_ssh_hook, device, ['env_ssh_hook.py', 'device', '--password-file', r'C:\v\pw'])
        self.assertEqual((0, True, 'PASS', [1, 1]), (code, report['ok'], report['status'], report['hook_lines']))
        self.assertEqual('PASS', windows_checks.verdict(code, json.dumps(report)))
        self.assertEqual({'cached_ms': 41, 'quiet_ms': 120, 'quiet_output_bytes': 0},
                         {key: report[key] for key in ('cached_ms', 'quiet_ms', 'quiet_output_bytes')})
        self.assertEqual(('env', 'vault', 'unlock', '--password-file', r'C:\v\pw', '--json'), device.calls[0])
        self.assertEqual(2, device.calls.count(('env', 'hook', 'powershell', '--install')))
        self.assertEqual(device.before, {path: data for path, data in device.disk.items() if data})
        self.assertEqual((None, True), (device.value, device.names_on))  # the probe is unset, the setting is back on
        self.assertNotIn(device.was_set, json.dumps(report))

    def test_env_ssh_hook_fails_when_uninstall_leaves_a_byte_behind_and_cleans_up_after_an_error(self):
        import env_ssh_hook
        import windows_checks
        device = ScriptedHookDevice(self, uninstall_keeps=b'\n')
        code, report = run_hook_check(env_ssh_hook, device, ['env_ssh_hook.py', 'device'])
        self.assertEqual((1, False, 'FAIL', False), (code, report['ok'], report['status'], report['uninstall_restores_rc']))
        self.assertEqual('FAIL', windows_checks.verdict(code, json.dumps(report)))
        broken = ScriptedHookDevice(self)
        broken.typed = lambda device, host, line: (_ for _ in ()).throw(RuntimeError('terminal closed'))
        error, _ = run_hook_check(env_ssh_hook, broken, ['env_ssh_hook.py', 'device'])
        self.assertEqual('terminal closed', str(error))
        self.assertEqual((None, False), (broken.value, broken.hooked()))  # unset and uninstalled all the same

    def test_env_ssh_hook_arguments_and_the_unix_path(self):
        import env_ssh_hook
        import windows_checks
        with patch.object(winhost, 'device', lambda: ScriptedHookDevice(self)), \
                patch.object(sys, 'argv', ['env_ssh_hook.py', 'another-host']), \
                patch.object(sys, 'stderr', __import__('io').StringIO()), self.assertRaises(SystemExit):
            env_ssh_hook.arguments()
        seen = []
        with patch.object(winhost, 'device', lambda: None), patch.object(sys, 'argv', ['env_ssh_hook.py', 'selfssh']), \
                patch.object(env_ssh_hook, 'ssh', lambda host, command, **options: seen.append((host, command)) or
                             subprocess.CompletedProcess([], 1, '', '')), self.assertRaisesRegex(RuntimeError, 'detect the host OS'):
            env_ssh_hook.main()
        self.assertEqual([('selfssh', 'uname -s')], seen)  # a Unix host is driven as before
        args = WindowsChecksRunner().args
        with patch.dict(os.environ, {'SMOKE_WINDOWS_SSH': 'me@winbox'}):
            table = windows_checks.checks(args(vault_password_file=r'C:\v\pw'))
        self.assertEqual(['env_ssh_hook.py', 'me@winbox', '--password-file', r'C:\v\pw'], table['env-ssh-hook'][0])
        self.assertEqual(['--vault_password_file'], windows_checks.missing(table['env-ssh-hook'][1], args()))

class FolderDevice:
    """A device whose files are a local directory: the RemotePath calls the config checks make, without PowerShell."""

    path_flavor = pathlib.PurePosixPath

    def __init__(self):
        self.calls = []

    def read_bytes(self, path):
        return pathlib.Path(str(path)).read_bytes()

    def write_bytes(self, path, data):
        pathlib.Path(str(path)).parent.mkdir(parents=True, exist_ok=True)
        pathlib.Path(str(path)).write_bytes(data)

    def exists(self, path):
        return pathlib.Path(str(path)).exists()

    def ps(self, step, script, **payload):
        self.calls.append((script, payload))
        if script != winhost.TREE:
            return 0, ''
        root = pathlib.Path(payload['path'])
        listed = [{'p': p.relative_to(root).as_posix(), 'h': __import__('hashlib').sha256(p.read_bytes()).hexdigest()}
                  for p in root.rglob('*') if p.is_file()]
        return 0, base64.b64encode(json.dumps(listed).encode()).decode()


class ConfigSyncPlumbing(unittest.TestCase):
    def setUp(self):
        import config_sync_lifecycle
        self.sync = config_sync_lifecycle

    @unittest.skipUnless(__import__('shutil').which('git'), 'needs git')
    def test_local_repository_is_mirrored_to_the_device_and_back_without_git_there(self):
        import tempfile
        sync, device = self.sync, FolderDevice()
        with tempfile.TemporaryDirectory() as here, tempfile.TemporaryDirectory() as there, \
                patch.object(sync, 'ROOT', pathlib.Path(here)), patch.object(sync, 'DEVICE', device), \
                patch.object(sync, 'MIRRORS', {}):
            bare = sync.new_bare('local', 'seed\n')
            copy = sync.on_device(bare, winhost.RemotePath(device, there))
            self.assertEqual(there + '/local.git', str(copy))
            self.assertFalse(list(pathlib.Path(str(copy)).glob('hooks/*')))

            def device_value(name='cfg/value.txt'):
                return subprocess.run(['git', '--git-dir', str(copy), 'show', 'main:' + name], capture_output=True,
                                      text=True).stdout
            self.assertEqual('seed\n', device_value())
            sync.edit_repository(bare, 'edited here\n')
            self.assertEqual('edited here\n', device_value())
            # pb on the device publishes into its copy: the next read and the next edit here start from it
            work = pathlib.Path(here) / 'machine'
            subprocess.run(['git', 'clone', '-q', str(copy), str(work)], check=True, capture_output=True)
            (work / 'cfg/value.txt').write_text('from machine\n')
            sync.git('commit', '-q', '-am', 'machine', cwd=work)
            sync.git('push', '-q', 'origin', 'main', cwd=work)
            self.assertEqual('from machine\n', sync.repository_value(bare))
            (work / 'other.txt').write_text('kept\n')
            sync.git('add', '-A', cwd=work)
            sync.git('commit', '-q', '-m', 'machine again', cwd=work)
            sync.git('push', '-q', 'origin', 'main', cwd=work)
            sync.edit_repository(bare, 'edited again\n')
            self.assertEqual(('edited again\n', 'kept\n'), (device_value(), device_value('other.txt')))

    def test_windows_steps_replace_chmod_systemctl_and_the_mode_check(self):
        import tomllib
        sync, calls = self.sync, []
        services = [{'Name': 'PaperboatRuntimeConfig-ab12', 'Status': 'Running', 'StartType': 'Automatic'}]
        device = SimpleNamespace(path_flavor=pathlib.PureWindowsPath, mkdir=lambda path: None,
                                 write_bytes=lambda path, data: calls.append(('write', str(path), data)),
                                 ps=lambda step, script, **payload: calls.append((script, payload)) or (0, ''),
                                 service=lambda action, pattern: calls.append((action, pattern)),
                                 services=lambda pattern: services, files=lambda directory: ['repository-1.json'],
                                 other_access=lambda path: calls.append(('acl', str(path))) or [])
        machine_file = winhost.RemotePath(device, r'C:\Users\u\AppData\Roaming\Paperboat\config-sync.toml')
        runtime = winhost.RemotePath(device, r'C:\Users\u\AppData\Local\Paperboat\runtime')
        managed = r'C:\Users\u\.cache\pb-cfg-lifecycle\managed.txt'
        with patch.object(sync, 'DEVICE', device), patch.object(sync, 'MACHINE_FILE', machine_file), \
                patch.object(sync, 'RUNTIME_ROOT', runtime):
            sync.write_machine_file('repo1', winhost.RemotePath(device, managed))
            self.assertEqual(0, sync.systemctl('stop', sync.UNIT).returncode)
            sync.systemctl('start', sync.UNIT)
            self.assertFalse(sync.worker_gone())
            services[0]['Status'] = 'Stopped'
            self.assertTrue(sync.worker_gone())
            del services[:]
            self.assertTrue(sync.worker_gone())
            self.assertTrue(sync.credentials_private())
            device.other_access = lambda path: ['S-1-1-0']
            self.assertFalse(sync.credentials_private())
            device.files = lambda directory: []
            self.assertFalse(sync.credentials_private())
        self.assertEqual(('write', str(machine_file)), calls[0][:2])
        parsed = tomllib.loads(calls[0][2].decode())
        self.assertEqual((managed, 'repo1'), (parsed['paths']['cfg/value.txt']['local_path'], parsed['pull']['repository_id']))
        self.assertEqual((winhost.PRIVATE, {'path': str(machine_file)}), calls[1])
        self.assertEqual([('stop', 'PaperboatRuntimeConfig*'), ('start', 'PaperboatRuntimeConfig*')], calls[2:4])
        self.assertEqual(('acl', str(runtime) + r'\config-sync\repository-credentials\repository-1.json'), calls[4])

    def test_unix_keeps_its_commands(self):
        import tempfile
        sync, ran = self.sync, []

        def run(argv, **options):
            ran.append((argv, options))
            return subprocess.CompletedProcess(argv, 0, 'active\n', '')
        self.assertIsNone(sync.DEVICE)
        with tempfile.TemporaryDirectory() as directory, patch('subprocess.run', run), \
                patch.object(sync, 'MACHINE_FILE', pathlib.Path(directory) / 'config-sync.toml'):
            sync.pb('config', 'status', '--json', stdin='x')
            sync.systemctl('stop', sync.UNIT)
            sync.write_machine_file('repo1', pathlib.Path('/home/u/.cache/pb-cfg-lifecycle/managed.txt'))
            self.assertEqual(0o600, sync.MACHINE_FILE.stat().st_mode & 0o777)
            self.assertIn('local_path = "/home/u/.cache/pb-cfg-lifecycle/managed.txt"', sync.MACHINE_FILE.read_text())
            self.assertEqual(8022, sync.Fixture().reach(8022))
        self.assertEqual(['pb', 'config', 'status', '--json'], ran[0][0])
        self.assertEqual({'input': 'x', 'stdin': None, 'capture_output': True, 'text': True, 'timeout': 180, 'env': None}, ran[0][1])
        self.assertEqual(['systemctl', '--user', 'stop', 'paperboat-runtime-config.service'], ran[1][0])

    def test_device_ssh_repository_is_this_hosts_sshd_through_a_forward(self):
        import contextlib
        import tempfile
        sync, calls, written = self.sync, [], {}

        @contextlib.contextmanager
        def forward(port):
            calls.append(port)
            yield 40022
        device = SimpleNamespace(path_flavor=pathlib.PureWindowsPath, forward=forward,
                                 write_bytes=lambda path, data: written.update({str(path): data}),
                                 ps=lambda step, script, **payload: calls.append((script, payload['path'])) or (0, ''))

        def run(argv, **options):
            if argv[0] == 'ssh-keygen':
                pathlib.Path(argv[argv.index('-f') + 1]).write_text('PRIVATE KEY\n')
                pathlib.Path(argv[argv.index('-f') + 1] + '.pub').write_text('ssh-ed25519 AAAA pb-cfg-lifecycle\n')
            return subprocess.CompletedProcess(argv, 0, '127.0.0.1 ssh-ed25519 HOSTKEY\n', '')
        root = r'C:\Users\u\.cache\pb-cfg-lifecycle'
        with tempfile.TemporaryDirectory() as here, patch.object(sync, 'ROOT', pathlib.Path(here) / 'root'), \
                patch.object(sync, 'HOME', pathlib.Path(here)), patch.object(sync, 'DEVICE', device), patch('subprocess.run', run):
            sync.ROOT.mkdir()
            fixture = sync.Fixture()
            key, known_hosts, host = fixture.enable_ssh(winhost.RemotePath(device, root))
            self.assertIn('pb-cfg-lifecycle', (pathlib.Path(here) / '.ssh/authorized_keys').read_text())
            fixture.close()
            self.assertEqual('', (pathlib.Path(here) / '.ssh/authorized_keys').read_text())
        self.assertEqual((root + r'\ssh_key', root + r'\known_hosts', '127.0.0.1:40022'), (str(key), str(known_hosts), host))
        self.assertEqual({str(key): b'PRIVATE KEY\n', str(known_hosts): b'[127.0.0.1]:40022 ssh-ed25519 HOSTKEY\n'}, written)
        self.assertEqual([22, (winhost.PRIVATE, str(key)), (winhost.PRIVATE, str(known_hosts))], calls)

    def test_device_helpers_send_the_private_acl_the_service_verb_and_the_forward_target(self):
        fake = FakeSsh((0, b''), (0, b''))
        device = winhost.WindowsDevice(target='user@host', pb_exe=EXE, password='secret')
        with patch('subprocess.run', fake):
            winhost.RemotePath(device, r'C:\x\key').private()
            device.service('stop', 'PaperboatRuntimeConfig*')
        self.assertEqual((winhost.PRIVATE, {'path': r'C:\x\key'}), (fake.calls[0]['script'], fake.calls[0]['payload']))
        self.assertIn("D:P(A;;FA;;;SY)(A;;FA;;;BA)", winhost.PRIVATE)  # configsync currentPrivateFileDescriptor
        self.assertEqual((winhost.SERVICE, {'action': 'stop', 'pattern': 'PaperboatRuntimeConfig*'}),
                         (fake.calls[1]['script'], fake.calls[1]['payload']))
        restarted = FakeSsh((0, b''))
        with patch('subprocess.run', restarted):
            device.service('restart', 'PaperboatHostd-x')
        self.assertEqual((winhost.SERVICE, {'action': 'restart', 'pattern': 'PaperboatHostd-x'}),
                         (restarted.calls[0]['script'], restarted.calls[0]['payload']))  # one snippet stops, then starts
        with self.assertRaises(AssertionError):
            device.service('pause', 'x')
        started = []

        class Tunnel:
            def poll(self):
                return None

            def terminate(self):
                pass

            def wait(self, timeout):
                pass
        with patch('subprocess.Popen', lambda argv, **kwargs: started.append(argv) or Tunnel()), \
                patch.object(device, 'listening', lambda port: True), device.forward(8000, '10.0.0.7') as port:
            self.assertIn('127.0.0.1:%d:10.0.0.7:8000' % port, started[0])

    def test_runner_names_the_config_sync_lifecycle_check(self):
        import windows_checks
        table = windows_checks.checks(WindowsChecksRunner().args())
        self.assertEqual((['config_sync_lifecycle.py', 'm'], ['--machine']), table['config-sync-lifecycle'])
        self.assertEqual(['--machine'], windows_checks.missing(['--machine'], WindowsChecksRunner().args(machine='')))


class ScriptedServerDevice(ScriptedHookDevice):
    """The same device with the VS Code server on it: each start keeps the environment its PowerShell had then."""

    DIR = r'C:\Users\u\.lanej-vscode-server'

    def __init__(self, test, fresh_sees_hook=True, reported_by_server=True):
        super().__init__(test)
        self.running, self.fetched, self.last = set(), None, 4000
        self.fresh_sees_hook, self.reported_by_server = fresh_sees_hook, reported_by_server

    def home(self):
        return winhost.RemotePath(self, r'C:\Users\u')

    def ps(self, step, script, **payload):
        import env_vscode_remote
        if script != env_vscode_remote.FETCH:
            return super().ps(step, script, **payload)
        self.fetched = payload
        return 0, ''

    def write_bytes(self, path, data):
        self.disk[str(path)] = data

    def unlink(self, path):
        self.disk = {name: data for name, data in self.disk.items() if not name.startswith(str(path))}

    def files(self, directory, pattern='*'):
        import fnmatch
        names = [name[len(str(directory)) + 1:] for name in self.disk if name.startswith(str(directory) + '\\')]
        return fnmatch.filter(names, pattern)

    def count_in_files(self, directory, pattern, text, recurse=False):
        self.test.assertEqual((self.DIR, 'server.log', 'Extension host agent'), (str(directory), pattern, text))
        return len(self.running)

    def processes(self, pattern):
        return [{'ProcessId': pid + (0 if self.reported_by_server else 1), 'Name': 'node.exe'} for pid in self.running]

    def stop(self, pid):
        self.running.discard(pid)

    def start(self, device, host):
        self.test.assertIn(self.DIR + r'\pb-env-probe.cjs', self.disk)  # the preload is there before the launch
        self.last += 1
        self.running.add(self.last)
        sees = self.hooked() and (self.fresh_sees_hook or self.last == 4001)
        self.disk[r'%s\env-%d.txt' % (self.DIR, self.last)] = self.sha().split()[1].encode() if sees else b'absent'
        return SimpleNamespace(poll=lambda: None, terminate=lambda: None, wait=lambda timeout: 0)

    def patches(self, module):
        return super().patches(module) + [patch.object(module, 'start_server_windows', self.start)]


class EnvVscodeRemote(unittest.TestCase):
    def test_the_server_started_before_the_hook_lacks_the_value_and_the_one_after_the_reload_has_it(self):
        import env_vscode_remote
        import windows_checks
        device = ScriptedServerDevice(self)
        code, report = run_hook_check(env_vscode_remote, device, ['env_vscode_remote.py', 'device', '--password-file', r'C:\v\pw'])
        self.assertEqual({'host': 'device', 'ok': True, 'status': 'PASS', 'server_started_before_hook_has_probe': False,
                          'server_after_reload_has_probe': True}, report)
        self.assertEqual('PASS', windows_checks.verdict(code, json.dumps(report)))
        self.assertEqual((device.DIR, 'server-win32-x64'), (device.fetched['dir'], device.fetched['url'].split('/')[-2]))
        self.assertEqual(('env', 'vault', 'unlock', '--password-file', r'C:\v\pw', '--json'), device.calls[0])
        # both servers are stopped, their folder and the hook are gone, the probe is unset
        self.assertEqual((set(), device.before, None), (device.running, {p: d for p, d in device.disk.items() if d}, device.value))

    def test_it_fails_when_the_reloaded_server_lacks_the_value_or_another_process_reported(self):
        import env_vscode_remote
        device = ScriptedServerDevice(self, fresh_sees_hook=False)
        code, report = run_hook_check(env_vscode_remote, device, ['env_vscode_remote.py', 'device'])
        self.assertEqual((1, 'FAIL', False), (code, report['status'], report['server_after_reload_has_probe']))
        device = ScriptedServerDevice(self, reported_by_server=False)
        error, _ = run_hook_check(env_vscode_remote, device, ['env_vscode_remote.py', 'device'])
        self.assertRegex(str(error), 'not reported by the one running server process')
        self.assertEqual((False, None), (device.hooked(), device.value))
        ended = SimpleNamespace(poll=lambda: 1)
        with self.assertRaisesRegex(RuntimeError, 'ended before it was listening'):
            env_vscode_remote.server_probe_windows(ScriptedServerDevice(self), winhost.RemotePath(device, device.DIR), ended)

    def test_runner_entry_and_the_unix_launch_are_as_before(self):
        import env_vscode_remote
        import windows_checks
        args = WindowsChecksRunner().args
        with patch.dict(os.environ, {'SMOKE_WINDOWS_SSH': 'me@winbox'}):
            table = windows_checks.checks(args(vault_password_file=r'C:\v\pw'))
        self.assertEqual(['env_vscode_remote.py', 'me@winbox', '--password-file', r'C:\v\pw'], table['env-vscode-remote'][0])
        self.assertEqual(['--vault_password_file'], windows_checks.missing(table['env-vscode-remote'][1], args()))
        self.assertIn("curl -fsSL 'https://update.code.visualstudio.com/latest/server-linux-x64/stable' | tar -xz",
                      env_vscode_remote.LAUNCH)
        self.assertIn('/proc/$pid/environ', env_vscode_remote.READ)

    @unittest.skipUnless(PWSH, 'set PB_TEST_PWSH to a pwsh binary to run the server launch for real')
    def test_the_launch_runs_the_server_in_the_hooked_shell_with_the_preload_and_a_log(self):
        import env_vscode_remote
        import tempfile
        with tempfile.TemporaryDirectory() as home:
            root = pathlib.Path(home) / '.lanej-vscode-server'
            (root / 'bin').mkdir(parents=True)
            server = root / 'bin' / 'code-server.cmd'  # stands in for the launcher: records what it was given
            server.write_text('#!/bin/sh\nprintf "%s|%s|%s" "$NODE_OPTIONS" "$PB_ENV_HOOK_PROBE" "$*" > "$(dirname "$0")/../seen"\n'
                              'echo "Extension host agent listening on 1"\n')
            server.chmod(0o755)
            profile = pathlib.Path(home) / 'powershell' / 'profile.ps1'
            profile.parent.mkdir()
            profile.write_text("$env:PB_ENV_HOOK_PROBE='from-profile'\n")
            ssh = pathlib.Path(home) / 'ssh.py'
            ssh.write_text(FAKE_SSH)
            device = SimpleNamespace(ssh_prefix=lambda *options: [sys.executable, str(ssh), PWSH, *options],
                                     ssh_environment=lambda: dict(os.environ, XDG_CONFIG_HOME=home, USERPROFILE=home))
            self.assertEqual(0, env_vscode_remote.start_server_windows(device, 'device').wait(timeout=60))
            options, probe, arguments = (root / 'seen').read_text().split('|')
            self.assertEqual('--require "%s/pb-env-probe.cjs"' % root, options)
            self.assertEqual('from-profile', probe)  # the server's shell loaded the profile
            self.assertEqual('--start-server --host=127.0.0.1 --port=0 --without-connection-token '
                             '--accept-server-license-terms --telemetry-level off', arguments)
            self.assertIn('Extension host agent', (root / 'server.log').read_text())
