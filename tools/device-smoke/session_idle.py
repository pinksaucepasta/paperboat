#!/usr/bin/env python3
"""F3-04: a detached session idle for 8 hours is still usable (same shell, variable intact).

`start` creates the session and records it; `verify` (run >= 8 h later) re-attaches and checks it.
Usage: session_idle.py start|verify TARGET [--state FILE] [--hours 8]
"""
import argparse
import json
from pathlib import Path
import time
import uuid

from session_cleanup import cleanup
from session_lifecycle import selected
from terminal_lifecycle import Terminal
import windows_target
import winhost

PB = winhost.pb_path()  # the client: this machine, or the Windows device (the state file stays here)


def attach(arguments):
    return Terminal(winhost.terminal_argv(PB, arguments), env=winhost.terminal_env(PB))


# A Windows target's session shell is cmd.exe under ConPTY: it announces no bracketed paste and has no printf, so the
# variable is a cmd one and every marker is a typed PowerShell line whose parent is that cmd.exe (its pid names the shell).
WIN_ID = "+'_'+$env:PB_IDLE+'_'+(Get-CimInstance Win32_Process -Filter ('ProcessId='+$PID)).ParentProcessId"


def windows_marker(terminal, token, nonce, timeout):
    """Type the marker line until it answers (a new or reattached ConPTY session is silent at first and an Enter typed
    into it can be lost: another Enter at a prompt only runs an empty line); returns the shell pid it printed."""
    line = windows_target.typed_marker(token, WIN_ID) + '\r'
    deadline = time.monotonic() + timeout
    terminal.send(line)
    while True:
        try:
            terminal.expect(token + '_' + nonce + '_', min(5, max(1, deadline - time.monotonic())))
            return terminal.expect('\n').decode().strip()
        except RuntimeError as error:
            if 'closed' in str(error) or time.monotonic() >= deadline:
                raise
            terminal.send('\r')


def establish(terminal, nonce, windows):
    """Sets the session's shell variable and returns the shell's pid."""
    if windows:
        deadline = time.monotonic() + 120
        while True:  # a new ConPTY session is silent at first and the first typed line can be lost: type it again
            terminal.send(windows_target.typed_marker('PB_READY') + '\r')
            try:
                terminal.expect('PB_READY', 10)
                break
            except RuntimeError as error:
                if 'closed' in str(error) or time.monotonic() >= deadline:
                    raise
        terminal.send('set PB_IDLE=%s\r' % nonce)
        return windows_marker(terminal, 'PB_SET', nonce, 60)
    terminal.expect(b'\x1b[?2004h')
    terminal.expect(terminal.marker('READY', 'stty -echo; '))
    terminal.send("PB_IDLE='%s'; pb_shell=$$; printf '\\nPB_SET_%%s_%%s\\n' \"$PB_IDLE\" \"$$\"\r" % nonce)
    terminal.expect('PB_SET_' + nonce + '_')
    return terminal.expect('\n').decode().strip()


def confirm(terminal, nonce, windows):
    """After the idle time: the shell variable and the shell's pid, as the reattached session reports them."""
    if windows:
        return windows_marker(terminal, 'PB_BACK', nonce, 90)
    terminal.expect(b'\x1b[?2004h', timeout=90)
    terminal.send("printf '\\nPB_BACK_%s_%s\\n' \"$PB_IDLE\" \"$$\"\r")
    terminal.expect('PB_BACK_' + nonce + '_', timeout=90)
    return terminal.expect('\n').decode().strip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('phase', choices=['start', 'verify'])
    parser.add_argument('target')
    parser.add_argument('--state', type=Path, default=Path.home() / '.pbsmoke-idle.json')
    parser.add_argument('--hours', type=float, default=8)
    args = parser.parse_args()
    if args.phase == 'start':
        name = 'smoke-idle-' + uuid.uuid4().hex[:10]
        nonce = uuid.uuid4().hex
        terminal = attach(['connect', args.target, '--name', name, '--status-bar', 'off'])
        try:
            shell = establish(terminal, nonce, windows_target.is_windows(PB, args.target))
        finally:
            terminal.close()
        _, session = selected(PB, args.target, name)
        args.state.write_text(json.dumps({'name': name, 'nonce': nonce, 'shell': shell, 'id': session['id'],
                                          'started': time.time()}))
        args.state.chmod(0o600)
        print(json.dumps({'check': 'session-idle', 'phase': 'start', 'status': 'STARTED', 'state': str(args.state)}), flush=True)
        return
    record = json.loads(args.state.read_text())
    idle = (time.time() - record['started']) / 3600
    if idle < args.hours:
        raise RuntimeError('only %.2f hours idle; need %.1f' % (idle, args.hours))
    terminal = attach(['session', 'attach', args.target, record['id'], '--status-bar', 'off'])
    try:
        if confirm(terminal, record['nonce'], windows_target.is_windows(PB, args.target)) != record['shell']:
            raise RuntimeError('idle session came back in a different shell')
    finally:
        terminal.close()
        cleanup(PB, args.target, record['name'])
        args.state.unlink(missing_ok=True)
    print(json.dumps({'check': 'session-idle', 'phase': 'verify', 'status': 'PASS', 'idle_hours': round(idle, 2),
                      'same_shell_and_variable': True}), flush=True)


if __name__ == '__main__':
    main()
