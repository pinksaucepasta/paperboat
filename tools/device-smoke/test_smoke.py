import os
import json
import pathlib
import subprocess
import tempfile
import time
import unittest
import socket
import socketserver
import threading

from install_path import windows_executable
from uninstall_inventory import retained
from pairing_fault import PairingFaultProxy

HERE = pathlib.Path(__file__).resolve().parent


class PairingNetworkFault(unittest.TestCase):
    def test_cut_closes_live_tunnel_and_same_proxy_accepts_recovery(self):
        class Echo(socketserver.BaseRequestHandler):
            def handle(self):
                self.request.settimeout(3)
                while True:
                    data = self.request.recv(128)
                    if not data:
                        return
                    self.request.sendall(data)

        class Server(socketserver.ThreadingTCPServer):
            daemon_threads = True

        server = Server(('127.0.0.1', 0), Echo)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        proxy = PairingFaultProxy(*server.server_address)
        def connect(target):
            connection = socket.create_connection(proxy.address, timeout=3)
            connection.sendall(('CONNECT ' + target + ' HTTP/1.1\r\n\r\n').encode())
            response = bytearray()
            while not response.endswith(b'\r\n\r\n'):
                value = connection.recv(1)
                if not value:
                    connection.close()
                    raise EOFError('proxy closed before its CONNECT response')
                response.extend(value)
            return connection, bytes(response)

        try:
            with self.subTest('endpoint denial'):
                connection, response = connect('other.example.test:443')
                with connection:
                    self.assertIn(b'403 Forbidden', response)
            connection, response = connect(proxy.authority)
            with connection:
                self.assertIn(b'200 Connection Established', response)
                connection.sendall(b'fixture-before-cut')
                self.assertEqual(connection.recv(128), b'fixture-before-cut')
                self.assertEqual(proxy.cut(), 1)
                self.assertEqual(connection.recv(128), b'')
            connection, response = connect(proxy.authority)
            with connection:
                self.assertIn(b'200 Connection Established', response)
                connection.sendall(b'fixture-after-cut')
                self.assertEqual(connection.recv(128), b'fixture-after-cut')
        finally:
            proxy.close()
            server.shutdown()
            server.server_close()
            thread.join(timeout=3)


class UninstallInventoryPreservation(unittest.TestCase):
    def test_preserved_inbox_does_not_hide_owned_state_or_symlinks(self):
        with tempfile.TemporaryDirectory() as directory:
            owned = pathlib.Path(directory) / "product"
            inbox = owned / "received" / "inbox"
            inbox.mkdir(parents=True)
            (inbox / "synthetic-smoke-file").write_bytes(b"fixture")
            self.assertFalse(retained(owned, inbox))
            credential = owned / "leftover-credential"
            credential.write_bytes(b"synthetic")
            self.assertTrue(retained(owned, inbox))
            credential.unlink()
            link = owned / "unexpected-link"
            link.symlink_to(inbox, target_is_directory=True)
            self.assertTrue(retained(owned, inbox))
            link.unlink()
            self.assertFalse(retained(owned, inbox))
            self.assertFalse(retained(owned / "missing", inbox))


class SmokeReceipts(unittest.TestCase):
    def test_launch_rejects_windows_target_before_credentials_or_device_access(self):
        environment = dict(os.environ)
        environment.pop("APPLE_PASS", None)
        environment.pop("VICTUS_PASS", None)
        for targets in ("victus", "hp,victus"):
            with self.subTest(targets=targets):
                result = subprocess.run(
                    ["bash", str(HERE / "smoke.sh"), "--expect-version", "fixture", "--targets", targets],
                    env=environment, capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode, 2)
                self.assertIn("Windows device smoke is deferred until after launch", result.stderr)

    def test_terminal_removes_only_its_exact_session_and_preserves_primary_failure(self):
        prefix = (HERE / 'smoke.sh').read_text().split('# ---------------------------------------------------------------- run')[0]
        for terminal_exit, fault in [(0, ""), (7, ""), (0, "foreign_scope"), (0, "failed_delete"), (0, "foreign_ack")]:
            with self.subTest(terminal_exit=terminal_exit, fault=fault), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                state = root / "state.json"
                state.write_text(json.dumps({"sessions": [], "mutations": []}))
                pb = root / "pb"
                pb.write_text(SESSION_PB_FIXTURE)
                pb.chmod(0o700)
                harness = root / "boundary.py"
                harness.write_text(SESSION_BOUNDARY_FIXTURE)
                script = root / "smoke.sh"
                script.write_text(prefix + r'''
HERE="$SUITE_DIR"
PB_HP="$FIXTURE_PB"
on() {
  shift
  case "$*" in
    python3*term.py*) REMOTE_COMMAND="$*" python3 "$BOUNDARY" terminal ;;
    python3*session_cleanup.py*) REMOTE_COMMAND="$*" python3 "$BOUNDARY" cleanup ;;
    *) cat >/dev/null ;;
  esac
}
test_terminal hp
exit "$FAILED"
''')
                result = subprocess.run(['bash', str(script), '--expect-version', 'test', '--targets', 'hp'],
                    env=dict(os.environ, SUITE_DIR=str(HERE), FIXTURE_PB=str(pb), BOUNDARY=str(harness),
                             FIXTURE_STATE=str(state), TERMINAL_EXIT=str(terminal_exit), SESSION_FAULT=fault),
                    capture_output=True, text=True, timeout=15)
                self.assertEqual(result.returncode, int(bool(terminal_exit or fault)), result.stdout + result.stderr)
                self.assertIn('cleanup-terminal:hp', result.stdout)
                final = json.loads(state.read_text())
                self.assertEqual([x['id'] for x in final['sessions'] if x['id'] != 'session_owned'],
                                 ['session_foreign', 'session_default'])
                if fault:
                    self.assertIn('FAIL', result.stdout)
                else:
                    self.assertNotIn('session_owned', [x['id'] for x in final['sessions']])
                    self.assertIn('delete:session_owned', final['mutations'])
                if fault == 'foreign_scope':
                    self.assertEqual(final['mutations'], [])
                self.assertNotIn('PRIVATE_SESSION_DETAIL', result.stdout + result.stderr)

    def test_preview_stops_its_exact_lease_and_cleanup_failure_fails_run(self):
        prefix = (HERE / 'smoke.sh').read_text().split('# ---------------------------------------------------------------- run')[0]
        for stop_exit, stop_id, stop_state, create_exit in [(0, "preview_owned", "stopped", 0), (7, "preview_owned", "stopped", 0),
                                                              (0, "preview_other", "stopped", 0), (0, "preview_owned", "ready", 0),
                                                              (0, "preview_owned", "stopped", 7)]:
            with self.subTest(stop_exit=stop_exit, stop_id=stop_id, stop_state=stop_state, create_exit=create_exit), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                curl = root / 'curl'
                curl.write_text('#!/bin/sh\necho pv-owned\n')
                curl.chmod(0o700)
                script = root / 'smoke.sh'
                script.write_text(prefix + '\nHERE=' + str(HERE) + r'''
RUN_ID=owned
on() {
  shift
  case "$*" in
    *'preview stop '*)
      printf '%s' "$*" > "$STOP_COMMAND"
      printf '{"schema":"paperboat.preview-tunnel/v1","kind":"preview_lease","id":"%s","state":"%s"}\n' "$STOP_ID" "$STOP_STATE"
      return "$STOP_EXIT" ;;
    *'--background'*)
      printf '%s\n' '{"schema":"paperboat.preview-tunnel/v1","kind":"preview_lease","id":"preview_owned","endpoint":"https://preview.example.test","state":"ready"}'
      return "$CREATE_EXIT" ;;
    *) return 0 ;;
  esac
}
test_preview hp
exit "$FAILED"
''')
                stopped = root / 'stop-command'
                result = subprocess.run(['bash', str(script), '--expect-version', 'test', '--targets', 'hp'],
                                        env=dict(os.environ, PATH=str(root) + os.pathsep + os.environ['PATH'],
                                                 STOP_COMMAND=str(stopped), STOP_EXIT=str(stop_exit), STOP_ID=stop_id, STOP_STATE=stop_state, CREATE_EXIT=str(create_exit)),
                                        capture_output=True, text=True, timeout=10)
                self.assertTrue(stopped.exists(), 'preview lease was left active')
                self.assertIn('preview stop preview_owned --json', stopped.read_text())
                self.assertEqual(result.returncode, int(stop_exit != 0 or stop_id != "preview_owned" or stop_state != "stopped" or create_exit != 0), result.stdout + result.stderr)
                self.assertIn('preview:hp', result.stdout)

    def run_exec(self, exit_code, forged_receipt=False, transport_failure=False):
        # Exercise the smoke owner through a local shell transport; the command
        # really runs and returns the requested status, including a forged stdout receipt.
        prefix = (HERE / 'smoke.sh').read_text().split('# ---------------------------------------------------------------- run')[0]
        with tempfile.TemporaryDirectory() as directory:
            script = pathlib.Path(directory) / 'smoke.sh'
            command = pathlib.Path(directory) / 'fake-pb'
            command.write_text('#!/usr/bin/env bash\necho "${@: -1}"\n[ "$FORGED" = 0 ] || echo rc=0\nexit "$COMMAND_EXIT"\n')
            command.chmod(0o700)
            script.write_text(prefix + f'\nPB_HP={command}\n' + '''
on() { shift; eval "$*"; [ "$TRANSPORT_FAIL" = 0 ]; }
test_exec hp
exit "$FAILED"
''')
            env = dict(os.environ, COMMAND_EXIT=str(exit_code),
                       FORGED=str(int(forged_receipt)), TRANSPORT_FAIL=str(int(transport_failure)))
            return subprocess.run(['bash', str(script), '--expect-version', 'test', '--targets', 'hp'],
                                  env=env, capture_output=True, text=True, timeout=10)

    def test_success_requires_command_and_transport_success(self):
        for code, forged, transport, expected in [(0, False, False, 0),
                                                  (7, True, False, 1),
                                                  (124, True, False, 1),
                                                  (0, False, True, 1)]:
            with self.subTest(code=code, forged=forged, transport=transport):
                result = self.run_exec(code, forged, transport)
                self.assertEqual(result.returncode, expected, result.stdout + result.stderr)
                self.assertIn('PASS' if expected == 0 else 'FAIL', result.stdout)

    def test_long_work_requires_elapsed_time_and_final_exit_receipt(self):
        prefix = (HERE / 'smoke.sh').read_text().split('# ---------------------------------------------------------------- run')[0]
        for duration, exit_code, transport, expected in [(50, 0, 0, 0), (1, 0, 0, 1),
                                                        (50, 7, 0, 1), (50, 0, 1, 1)]:
            with self.subTest(duration=duration, exit_code=exit_code, transport=transport):
                with tempfile.TemporaryDirectory() as directory:
                    root = pathlib.Path(directory)
                    (root / 'job.start').write_text('100')
                    (root / 'job.end').write_text(str(100 + duration))
                    (root / 'job.out').write_text(f'FINAL\nrc=0\nrc={exit_code}\n')
                    script = root / 'smoke.sh'
                    script.write_text(prefix + r'''
RUN_ID=owned
on() { shift; local command="${*//\/tmp\/owned/$JOB_DIR}"; eval "$command"; [ "$TRANSPORT_FAIL" = 0 ]; }
collect long job FINAL 50
exit "$FAILED"
''')
                    # A failed transport would keep the readiness poll waiting;
                    # fail only the output retrieval to test that boundary.
                    script.write_text(script.read_text().replace('[ "$TRANSPORT_FAIL" = 0 ];',
                                      '[[ "$command" == test* ]] || [ "$TRANSPORT_FAIL" = 0 ];'))
                    result = subprocess.run(['bash', str(script), '--expect-version', 'test', '--targets', 'hp'],
                                            env=dict(os.environ, JOB_DIR=directory, TRANSPORT_FAIL=str(transport)),
                                            capture_output=True, text=True, timeout=10)
                    self.assertEqual(result.returncode, expected, result.stdout + result.stderr)


class TerminalDriver(unittest.TestCase):
    def test_failure_is_nonzero_and_private_output_is_withheld(self):
        driver = (HERE / 'smoke.sh').read_text().split("TERM_DRIVER=$(cat <<'PY'\n", 1)[1].split("\nPY\n)", 1)[0]
        with tempfile.TemporaryDirectory() as directory:
            command = pathlib.Path(directory) / 'pb'
            command.write_text('#!/bin/sh\necho PRIVATE_SENTINEL\nexit 7\n')
            command.chmod(0o700)
            result = subprocess.run(['python3', '-c', driver, str(command), 'hp', 'owned',
                                     'echo computed-$((6*7))', 'computed-42'],
                                    capture_output=True, text=True, timeout=20)
            self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
            self.assertEqual(result.stdout.strip(), 'TERMINAL_FAIL')
            self.assertNotIn('PRIVATE_SENTINEL', result.stdout + result.stderr)

    def test_computed_answer_and_forced_child_cleanup(self):
        driver = (HERE / 'smoke.sh').read_text().split("TERM_DRIVER=$(cat <<'PY'\n", 1)[1].split("\nPY\n)", 1)[0]
        with tempfile.TemporaryDirectory() as directory:
            command = pathlib.Path(directory) / 'pb'
            pid_file = pathlib.Path(directory) / 'pid'
            command.write_text('#!/bin/sh\ntrap "" TERM\necho $$ > "$PID_FILE"\nwhile read -r line; do [ "$line" = exit ] || eval "$line"; done\n')
            command.chmod(0o700)
            ready, resume = pathlib.Path(directory) / 'ready', pathlib.Path(directory) / 'resume'
            process = subprocess.Popen(['python3', '-c', driver, str(command), 'hp', 'owned',
                                        'pb_smoke=$$; echo computed-$((6*7))', 'computed-42',
                                        str(ready), str(resume),
                                        '[ "$pb_smoke" = "$$" ] && echo preserved-$((6*7))', 'preserved-42'],
                                       env=dict(os.environ, PID_FILE=str(pid_file)),
                                       stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            try:
                deadline = time.monotonic() + 15
                while not ready.exists() and process.poll() is None and time.monotonic() < deadline:
                    time.sleep(0.05)
                self.assertTrue(ready.exists(), 'held terminal never acknowledged its first command')
                self.assertIsNone(process.poll(), 'terminal ended before activation completed')
                resume.touch()
                stdout, stderr = process.communicate(timeout=15)
                result = subprocess.CompletedProcess(process.args, process.returncode, stdout, stderr)
            finally:
                if process.poll() is None:
                    process.kill()
                    process.communicate()
                    if pid_file.exists():
                        try:
                            os.kill(int(pid_file.read_text()), 9)
                        except ProcessLookupError:
                            pass
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertEqual(result.stdout.strip(), 'TERMINAL_OK')
            with self.assertRaises(ProcessLookupError):
                os.kill(int(pid_file.read_text()), 0)


class WindowsInstallReceipt(unittest.TestCase):
    def test_custom_install_directory_is_preserved(self):
        path = "D:\\Owner's tools\\pb.exe"
        self.assertEqual(windows_executable({'schema_version': '1.0', 'ok': True, 'data': {'executable': path}}), path)

    def test_missing_relative_and_malformed_paths_fail(self):
        for receipt in [{}, {'ok': False, 'data': {'executable': 'C:\\tools\\pb.exe'}},
                        {'schema_version': '2.0', 'ok': True, 'data': {'executable': 'C:\\tools\\pb.exe'}},
                        *({'schema_version': '1.0', 'ok': True, 'data': {'executable': value}}
                          for value in ['pb.exe', 'C:\\tools\\other.exe', 'C:\\tools\\pb.exe\nignored', 7])]:
            with self.subTest(receipt=receipt), self.assertRaises(ValueError):
                windows_executable(receipt)




SESSION_BOUNDARY_FIXTURE = r'''
import json, os, shlex, subprocess, sys
from pathlib import Path
path=Path(os.environ['FIXTURE_STATE'])
args=shlex.split(os.environ['REMOTE_COMMAND'])
if sys.argv[1]=='terminal':
    path.write_text(json.dumps({'sessions':[
      {'id':'session_owned','name':args[4],'state':'running','is_default':False},
      {'id':'session_foreign','name':'unrelated','state':'running','is_default':False},
      {'id':'session_default','name':'default','state':'running','is_default':True}], 'mutations':[]}))
    print('TERMINAL_FAIL' if int(os.environ['TERMINAL_EXIT']) else 'TERMINAL_OK')
    sys.exit(int(os.environ['TERMINAL_EXIT']))
sys.exit(subprocess.run([sys.executable,os.environ['SUITE_DIR']+'/session_cleanup.py',*args[2:]]).returncode)
'''

SESSION_PB_FIXTURE = r'''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
path=Path(os.environ['FIXTURE_STATE'])
state=json.loads(path.read_text())
action=sys.argv[2]
fault=os.environ['SESSION_FAULT']
environment={'id':'machine_owned'}
if action=='list':
    print(json.dumps({'version':'1','environment':environment,'sessions':state['sessions']}))
    sys.exit(0)
session=sys.argv[4]
if session!='session_owned':
    print('PRIVATE_SESSION_DETAIL',file=sys.stderr)
    sys.exit(9)
if '--confirm' not in sys.argv:
    scope='session-'+action+':machine_owned:'+session
    if fault=='foreign_scope': scope='session-close:machine_foreign:session_other'
    print(json.dumps({'ok':False,'data':{'outcome':'confirmation_required','scope':scope,'confirmation_token':'ABC123'},
                      'error':{'code':'confirmation_required'}}))
    sys.exit(2)
state['mutations'].append(action+':'+session)
if action=='close':
    state['sessions'][0]['state']='closed'
    receipt={'state':'closed'}
else:
    receipt={'deleted':True}
    if fault!='failed_delete': state['sessions']=[s for s in state['sessions'] if s['id']!=session]
path.write_text(json.dumps(state))
print(json.dumps({'version':'1','environment':environment,
                  'session_id':'session_other' if fault=='foreign_ack' else session,**receipt}))
sys.exit(7 if action=='delete' and fault=='failed_delete' else 0)
'''

if __name__ == '__main__':
    unittest.main()
