#!/usr/bin/env python3
"""Prove timeout and Ctrl-C stop the actual remote process, with known outcomes."""
import argparse
import base64
import json
import subprocess
import time
import uuid

from exec_lifecycle import run
from install_lifecycle import registration
from pairing_lifecycle import command_diagnostic
import winhost


class FixtureCommandFailure(RuntimeError):
    def __init__(self, diagnostic):
        self.diagnostic_path = diagnostic
        self.cleanup_errors = []
        super().__init__('exec cancellation check failed; private diagnostics: ' + str(diagnostic))


def events(data):
    if len(data) > 1 << 20:
        raise RuntimeError('exec cancellation output exceeded its bound')
    complete = data[:data.rfind(b'\n') + 1]
    return [json.loads(line) for line in complete.splitlines() if line.strip()]


def process_identity(output, nonce):
    data = b''.join(base64.b64decode(event['data']) for event in events(output.stdout())
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
    process = winhost.job(binary, arguments[1:])
    try:
        deadline = time.monotonic() + 35
        while pid is None and time.monotonic() < deadline:
            pid = process_identity(process, nonce)
            if pid is None and process.poll() is not None:
                break
            time.sleep(.1)
        if pid is None:
            raise RuntimeError('exec cancellation fixture did not establish its remote process')
        if mode == 'interrupt':
            process.interrupt()
        status = process.wait(35)
        expected = 124 if mode == 'timeout' else 130
        terminal = [event for event in events(process.stdout()) if event.get('state') == 'canceled']
        wanted = 'exec_timeout' if mode == 'timeout' else 'exec_canceled'
        if status != expected or len(terminal) != 1 or terminal[0].get('error_code') != wanted:
            raise RuntimeError('cancellation lost its known outcome')
        verify_stopped(binary, target, pid, nonce)
    except Exception as error:
        diagnostic = command_diagnostic(arguments, process.stdout(1 << 20), process.stderr())
        failure = FixtureCommandFailure(diagnostic)
        raise failure from error
    finally:
        if process.poll() is None:
            try:
                process.interrupt()
                process.wait(35)
            except (RuntimeError, subprocess.TimeoutExpired):
                process.kill()
        process.close()
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
                failure.cleanup_errors.append(cleanup_error)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    before = registration()
    binary = winhost.pb_path()
    modes = ['timeout'] if winhost.device() else ['timeout', 'interrupt']  # no Ctrl-C reaches a hidden Windows process
    for mode in modes:
        one(binary, args.target, mode)
    if registration() != before:
        raise RuntimeError('exec cancellation changed the enrolled identity')
    print(json.dumps({'check': 'exec-cancel', 'target': args.target, 'status': 'PASS',
                      'phases': [m if m == 'timeout' else 'ctrl_c' for m in modes] + ['remote_process_stopped'], 'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
