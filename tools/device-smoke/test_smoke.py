import os
import pathlib
import subprocess
import tempfile
import time
import unittest

from install_path import windows_executable

HERE = pathlib.Path(__file__).resolve().parent


class SmokeReceipts(unittest.TestCase):
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


if __name__ == '__main__':
    unittest.main()
