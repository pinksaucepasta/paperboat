#!/usr/bin/env python3
"""F14-02/F14-04: an update refuses while a `pb ssh` connection is open, keeps a held terminal and a running exec, then retires the old session owner.

Usage: update_continuity.py TARGET CANDIDATE (sudo password on stdin). CANDIDATE is a different build.
A shell is held open in a real PTY, an exec is running, `pb update --from CANDIDATE` runs, and
afterwards the SAME shell answers without user action, the exec finished normally, and the old
generation's session owner exits once its last session is closed.
"""
import argparse
import json
from pathlib import Path
import os
import subprocess
import sys
import time

import pairing_lifecycle as p
from session_cleanup import cleanup
from terminal_lifecycle import Terminal
import windows_target
import winhost

PB = winhost.pb_path()


# PowerShell appended to a typed marker: the id of the cmd.exe this line was typed into.
SHELL_PID = "+(Get-CimInstance Win32_Process -Filter ('ProcessId='+$PID)).ParentProcessId"


def owners():
    if winhost.device():  # each generation's session owner is a Windows service named after it
        return {s['Name']: [s['Name']] for s in winhost.device().services('PaperboatSessionOwner-*') if s['Status'] == 'Running'}
    # ps, not pgrep -af (macOS pgrep prints no arguments with -a); only this user's owners: another account
    # on the host keeps its own owner running for good
    out = subprocess.run(['ps', '-U', str(os.getuid()), '-o', 'pid=,args='], capture_output=True, text=True).stdout
    found = {}
    for line in out.splitlines():
        parts = line.split()
        if '__runtime-session-owner' in parts and '--generation' in parts:
            found.setdefault(parts[parts.index('--generation') + 1], []).append(parts[0])
    return found


def wait(predicate, seconds, step):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(3)
    raise RuntimeError(step + ' did not happen within ' + str(seconds) + 's')


def ssh_blocks_update(target, candidate, password):
    """F14-02 (user decision): an update never silently ends an open `pb ssh` connection.

    With a connection open, `pb update --from` refuses (busy, naming the connection) and changes nothing;
    once it is closed the update is allowed again (the caller runs it next)."""
    import re
    import select
    version = p.invoke(PB, '--version', raw=True)
    # OpenSSH joins remote arguments with spaces, so the remote command is one string.
    # A Windows target's sshd hands the command to cmd.exe: ping is its sleep.
    held = 'echo PB_SSH_UP& ping -n 600 127.0.0.1 >nul' if windows_target.is_windows(PB, target) else 'echo PB_SSH_UP; sleep 600'
    ssh = winhost.job(PB, ['ssh', target, '--', held])
    try:
        deadline = time.monotonic() + 90
        while b'PB_SSH_UP' not in ssh.stdout():
            if time.monotonic() > deadline or ssh.poll() is not None:
                raise RuntimeError('the held pb ssh connection did not come up')
            time.sleep(1)
        refused = p.command_result(PB, 'update', '--from', candidate, '--json', timeout=120,
                                   terminal=True, password=password)
        text = re.sub(r'\x1b\[[0-9;?]*[a-zA-Z]', '', refused.stdout)
        if not refused.returncode or 'open SSH connection' not in text or 'update_busy' not in text or '"state_changed":false' not in text.replace(' ', ''):
            raise RuntimeError('update with an open pb ssh connection was not refused as busy: rc=%s %s' % (refused.returncode, text[-400:]))
        if p.invoke(PB, '--version', raw=True) != version:
            raise RuntimeError('a refused update changed the installed version')
        if ssh.poll() is not None:
            raise RuntimeError('the refused update ended the pb ssh connection')
    finally:
        ssh.terminate()
        ssh.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('candidate')
    parser.add_argument('--hold', choices=['both', 'terminal', 'exec'], default='both')
    parser.add_argument('--exec-seconds', type=int, default=45)
    args = parser.parse_args()
    password = None if winhost.device() else sys.stdin.readline().rstrip('\n')
    windows = windows_target.is_windows(PB, args.target)
    name = 'smoke-update-' + str(int(time.time()))
    before_version = p.invoke(PB, '--version', raw=True)
    terminal = Terminal(winhost.terminal_argv(PB, ['connect', args.target, '--name', name, '--status-bar', 'off']),
                        env=winhost.terminal_env(PB))
    exec_process = None

    def shell_identity(label):
        """Ask the held shell for its process id: $$ in sh; under cmd.exe the parent of a PowerShell typed there."""
        if windows:
            terminal.send(windows_target.typed_marker('PB_' + label + '_', SHELL_PID) + '\r')
        else:
            terminal.send("printf '\\nPB_" + label + "''_%s\\n' \"$$\"\r")
        terminal.expect('PB_' + label + '_', timeout=180)
        return terminal.expect('\n', timeout=10).decode().strip()
    try:
        if not windows:
            terminal.expect(b'\x1b[?2004h')
            terminal.expect(terminal.marker('READY', 'stty -echo; '))
        held_pid = shell_identity('SHELL')
        if not held_pid.isdigit():
            raise RuntimeError('could not read the held shell identity')
        held = set(owners())
        if not held:
            raise RuntimeError('no session owner is running for the held terminal')
        if args.hold != 'terminal':
            exec_process = winhost.job(PB, ['exec', args.target, '--', *(windows_target.ps_argv(
                "Start-Sleep %d;[Console]::Out.Write('EXEC_FINISHED')" % args.exec_seconds) if windows else
                ['sh', '-c', 'sleep %d; echo EXEC_FINISHED' % args.exec_seconds])])
        ssh_blocks_update(args.target, args.candidate, password)
        time.sleep(5)
        for attempts in (1, 2, 3):  # the closed pb ssh connection may still be winding down; the attempt count is reported
            result = p.command_result(PB, 'update', '--from', args.candidate, '--json', timeout=420,
                                      terminal=True, password=password)
            if not result.returncode:
                break
            time.sleep(15)
        if result.returncode:
            import re
            raise RuntimeError('update failed while work was held rc=%s: %s' % (result.returncode, re.sub(r'\x1b\[[0-9;?]*[a-zA-Z]', '', result.stdout)[-2500:]))
        after_version = p.invoke(PB, '--version', raw=True)
        if after_version == before_version:
            raise RuntimeError('candidate must be a different build')
        # Without any user action the same shell answers.
        again_pid = shell_identity('AGAIN')
        if again_pid != held_pid:
            raise RuntimeError('terminal reconnected to a different shell')
        if exec_process is not None:
            code = exec_process.wait(180)
            out, err = exec_process.stdout().decode(errors='replace'), exec_process.stderr().decode(errors='replace')
            if code != 0 or 'EXEC_FINISHED' not in out:
                raise RuntimeError('running exec did not finish normally across the update: rc=%s stdout=%r stderr=%r' % (code, out[-120:], err[-300:]))
            exec_process.close()
            exec_process = None
        current = owners()
        if not held & set(current):
            raise RuntimeError('old session owner vanished while its session was still open')
        terminal.send('exit\r')
        terminal.wait_exit(30)
        cleanup(PB, args.target, name)
        wait(lambda: not held & set(owners()), 180, 'old session owner retirement')
    finally:
        if exec_process is not None:
            exec_process.kill()
            exec_process.close()
        terminal.close()
        try:
            cleanup(PB, args.target, name)
        except Exception:
            pass
    print(json.dumps({'check': 'update-continuity', 'target': args.target, 'status': 'PASS', 'update_attempts': attempts,
                      'same_shell_after_update': True, 'exec_finished': True,
                      'old_owner_retired': True, 'busy_while_ssh_open': True}), flush=True)


if __name__ == '__main__':
    main()
