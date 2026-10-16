#!/usr/bin/env python3
"""Prove timeout and Ctrl-C stop the actual remote process, with known outcomes."""
import argparse
import base64
import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile
import time
import uuid

from exec_lifecycle import run
from install_lifecycle import registration
from pairing_lifecycle import command_diagnostic


def events(output):
    data = os.pread(output.fileno(), (1 << 20) + 1, 0)
    if len(data) > 1 << 20:
        raise RuntimeError('exec cancellation output exceeded its bound')
    complete = data[:data.rfind(b'\n') + 1]
    return [json.loads(line) for line in complete.splitlines() if line.strip()]


def process_identity(output, nonce):
    data = b''.join(base64.b64decode(event['data']) for event in events(output)
                    if event.get('stream') == 'stdout')
    for line in data.splitlines():
        parts = line.decode().split()
        if len(parts) == 2 and parts[0] == nonce:
            return int(parts[1])
    return None


def verify_stopped(binary, target, pid, nonce):
    code = ("import os,subprocess,time\n"
            "pid=" + str(pid) + "\nnonce=" + repr(nonce) + "\n"
            "deadline=time.monotonic()+5\n"
            "while time.monotonic()<deadline:\n"
            " command=subprocess.run(['ps','-p',str(pid),'-o','command='],capture_output=True,text=True).stdout\n"
            " if nonce not in command: break\n"
            " time.sleep(.1)\n"
            "else: raise RuntimeError('canceled remote process remains alive')\n")
    run(binary, 'exec', target, '--timeout', '15s', '--', 'python3', '-c', code)


def one(binary, target, mode):
    nonce = 'PB_EXEC_CANCEL_' + uuid.uuid4().hex
    code = "import os,sys,time; print(sys.argv[1],os.getpid(),flush=True); time.sleep(120)"
    arguments = [str(binary), 'exec', target, '--timeout', '15s' if mode == 'timeout' else '2m',
                 '--json', '--', 'python3', '-c', code, nonce]
    pid = None
    failure = None
    with tempfile.TemporaryFile() as output, tempfile.TemporaryFile() as errors:
        process = subprocess.Popen(arguments, stdin=subprocess.DEVNULL, stdout=output, stderr=errors)
        try:
            deadline = time.monotonic() + 35
            while pid is None and time.monotonic() < deadline:
                pid = process_identity(output, nonce)
                if pid is None and process.poll() is not None:
                    break
                time.sleep(.1)
            if pid is None:
                raise RuntimeError('exec cancellation fixture did not establish its remote process')
            if mode == 'interrupt':
                process.send_signal(signal.SIGINT)
            status = process.wait(timeout=35)
            expected = 124 if mode == 'timeout' else 130
            terminal = [event for event in events(output) if event.get('state') == 'canceled']
            wanted = 'exec_timeout' if mode == 'timeout' else 'exec_canceled'
            if status != expected or len(terminal) != 1 or terminal[0].get('error_code') != wanted:
                output.seek(0); errors.seek(0)
                diagnostic = command_diagnostic(arguments, output.read(), errors.read())
                raise RuntimeError('cancellation lost its known outcome; private diagnostics: ' + str(diagnostic))
            verify_stopped(binary, target, pid, nonce)
        except Exception as error:
            failure = error
            raise
        finally:
            if process.poll() is None:
                process.send_signal(signal.SIGINT)
                try:
                    process.wait(timeout=35)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
            if pid is not None:
                # Exact nonce guards the PID against reuse; only our synthetic child is removed.
                cleanup = ("import os,signal,subprocess\npid=" + str(pid) + "\n"
                           "command=subprocess.run(['ps','-p',str(pid),'-o','command='],capture_output=True,text=True).stdout\n"
                           "if " + repr(nonce) + " in command: os.kill(pid,signal.SIGKILL)\n")
                try:
                    run(binary, 'exec', target, '--timeout', '15s', '--', 'python3', '-c', cleanup)
                except Exception as cleanup_error:
                    if failure is None:
                        raise
                    failure.add_note('Exact exec fixture cleanup failed: ' + str(cleanup_error))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    before = registration()
    binary = Path.home() / '.local/bin/pb'
    for mode in ['timeout', 'interrupt']:
        one(binary, args.target, mode)
    if registration() != before:
        raise RuntimeError('exec cancellation changed the enrolled identity')
    print(json.dumps({'check': 'exec-cancel', 'target': args.target, 'status': 'PASS',
                      'phases': ['timeout', 'ctrl_c', 'remote_process_stopped'], 'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
