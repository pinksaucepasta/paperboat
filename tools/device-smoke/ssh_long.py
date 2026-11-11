#!/usr/bin/env python3
"""Hold one managed SSH session for over an hour and require its final marker and exit status (F5-02).

--interactive holds a real pty session (the shell of `pb ssh <m>` under ConPTY/pty, output every minute, then `exit 0`)."""
import argparse
import json
import time
import uuid

from install_lifecycle import registration
import windows_target
from terminal_lifecycle import Terminal
from ssh_lifecycle import shell_ready
import winhost


def hold_interactive(binary, args, marker, command):
    """One interactive session on a pty held for args.seconds: the long command is typed into the shell, its output
    every minute keeps the session busy, the final marker (never typed verbatim, so the echo cannot match) and `exit 0`
    must follow."""
    if windows_target.is_windows(binary, args.target):
        raise RuntimeError('--interactive holds a Unix shell target')
    prefix, tail = marker[:-8], marker[-8:]
    typed = command.replace("printf '%s\\n' " + marker, "printf '%s%s\\n' " + prefix + ' ' + tail)
    terminal = Terminal(winhost.terminal_argv(binary, ['ssh', args.target]), env=winhost.terminal_env(binary))
    try:
        shell_ready(terminal)
        terminal.send(typed + '\r')
        terminal.expect(marker, timeout=args.seconds + 300)
        terminal.send('exit 0\r')
        if terminal.wait_exit(30) != 0:
            raise RuntimeError('the interactive SSH session did not exit 0 after being held')
    finally:
        terminal.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--seconds', type=int, default=3601)
    parser.add_argument('--interactive', action='store_true')
    args = parser.parse_args()
    binary = winhost.pb_path()
    before = registration()
    marker = 'PB_SSH_LONG_' + uuid.uuid4().hex
    # The session prints once a minute so idle timeouts on any hop would show, then the marker at the end.
    command = (f"i=0; while [ $i -lt {args.seconds} ]; do sleep 60; i=$((i+60)); printf 't%s\\n' $i; done; "
               f"printf '%s\\n' {marker}")
    if windows_target.is_windows(binary, args.target):  # cmd.exe there has no sh loop: the same lines from PowerShell
        command = windows_target.ssh_command(windows_target.long_script(args.seconds, marker))
    if args.interactive:
        hold_interactive(binary, args, marker, command)
        if registration() != before:
            raise RuntimeError('SSH changed the enrolled identity')
        print(json.dumps({'check': 'ssh-long', 'target': args.target, 'status': 'PASS', 'seconds': args.seconds,
                          'interactive': True, 'final_marker': True, 'enrollment_unchanged': True}))
        return
    result = winhost.run_pb(binary, ['ssh', args.target, '--', command], args.seconds + 300)
    if result.returncode != 0 or not result.stdout.decode(errors='replace').rstrip().endswith(marker):
        raise RuntimeError(f'the long SSH session ended with exit {result.returncode} before its final marker: '
                           + result.stderr.decode(errors='replace')[-300:])
    if registration() != before:
        raise RuntimeError('SSH changed the enrolled identity')
    print(json.dumps({'check': 'ssh-long', 'target': args.target, 'status': 'PASS', 'seconds': args.seconds,
                      'final_marker': True, 'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
