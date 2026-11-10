#!/usr/bin/env python3
"""Prove timeout and Ctrl-C stop the actual remote process, with known outcomes."""
import argparse
import base64
import json
import contextlib
import subprocess
import time
import uuid

from exec_lifecycle import run
from install_lifecycle import registration
from pairing_lifecycle import command_diagnostic
import windows_target
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
    data = b''.join(base64.b64decode(event['data']) for event in events(output)
                    if event.get('stream') == 'stdout')
    for line in data.splitlines():
        parts = line.decode().split()
        if len(parts) == 2 and parts[0] == nonce:
            return int(parts[1])
    return None


def windows_interrupt(binary, arguments, nonce):
    """Send real console Ctrl-C only after exec has published the remote process identity."""
    device = binary.device
    with device.temp_dir('pb-exec-cancel') as directory:
        output = winhost.RemotePath(device, directory) / 'stdout'
        errors = winhost.RemotePath(device, directory) / 'stderr'
        when = ("$text='';if(Test-Path -LiteralPath $req.stdout){"
                "foreach($line in [IO.File]::ReadLines($req.stdout)){try{"
                "$e=$line|ConvertFrom-Json;if($e.stream -eq 'stdout'){"
                "$text += [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($e.data))}"
                "}catch{}}};$text -match '" + nonce + " [0-9]+'")
        fired, status = device.interrupt_when(arguments, when, seconds=35, exit_seconds=35,
                                              stdout=output, stderr=errors)
        stdout, stderr = output.read_bytes(), errors.read_bytes()
        if not fired:
            raise FixtureCommandFailure(command_diagnostic(arguments, stdout, stderr))
        return status, stdout, stderr


def verify_stopped(binary, target, pid, nonce):
    if windows_target.is_windows(binary, target):
        run(binary, 'exec', target, '--timeout', '15s', '--', *windows_target.ps_argv(windows_target.stopped_script(pid, nonce)))
        return
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
    stack = contextlib.ExitStack()  # holds the staged PowerShell fixture on a Windows target
    windows = False
    arguments = [str(binary), 'exec', target]
    pid = None
    failure = None
    process = None
    output, errors = b'', b''
    try:
        # Everything that can fail before the child runs is inside this try, so every startup failure keeps its
        # private diagnostic (the platform lookup and the staging run pb too).
        windows = windows_target.is_windows(binary, target)
        if windows:
            fixture = windows_target.file_argv(
                stack.enter_context(windows_target.staged(binary, target, windows_target.sleeper_script())), nonce)
        else:
            code = "import os,sys,time; print(sys.argv[1],os.getpid(),flush=True); time.sleep(120)"
            fixture = ['python3', '-c', code, nonce]
        arguments = [str(binary), 'exec', target, '--timeout', '15s' if mode == 'timeout' else '2m',
                     '--json', '--', *fixture]
        if mode == 'interrupt' and hasattr(binary, 'device'):
            status, output, errors = windows_interrupt(binary, arguments, nonce)
            pid = process_identity(output, nonce)
        else:
            process = winhost.job(binary, arguments[1:])
            deadline = time.monotonic() + 35
            while pid is None and time.monotonic() < deadline:
                pid = process_identity(process.stdout(), nonce)
                if pid is None and process.poll() is not None:
                    break
                time.sleep(.1)
            if mode == 'interrupt':
                process.interrupt()
            status = process.wait(35)
            output = process.stdout()
        if pid is None:
            raise RuntimeError('exec cancellation fixture did not establish its remote process')
        expected = 124 if mode == 'timeout' else 130
        terminal = [event for event in events(output) if event.get('state') == 'canceled']
        wanted = 'exec_timeout' if mode == 'timeout' else 'exec_canceled'
        if status != expected or len(terminal) != 1 or terminal[0].get('error_code') != wanted:
            raise RuntimeError('cancellation lost its known outcome')
        verify_stopped(binary, target, pid, nonce)
    except Exception as error:
        diagnostic = getattr(error, 'diagnostic_path', None) or command_diagnostic(
            arguments, process.stdout(1 << 20) if process else output,
            process.stderr() if process else errors + str(error).encode())
        failure = FixtureCommandFailure(diagnostic)
        raise failure from error
    finally:
        if process is not None:
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
                run(binary, 'exec', target, '--timeout', '15s', '--',
                    *(windows_target.ps_argv(windows_target.kill_script(pid, nonce)) if windows else ['python3', '-c', cleanup]))
            except Exception as cleanup_error:
                if failure is None:
                    raise
                failure.cleanup_errors.append(cleanup_error)
        try:
            stack.close()
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
    modes = ['timeout', 'interrupt']
    for mode in modes:
        one(binary, args.target, mode)
    if registration() != before:
        raise RuntimeError('exec cancellation changed the enrolled identity')
    print(json.dumps({'check': 'exec-cancel', 'target': args.target, 'status': 'PASS',
                      'phases': [m if m == 'timeout' else 'ctrl_c' for m in modes] + ['remote_process_stopped'], 'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
