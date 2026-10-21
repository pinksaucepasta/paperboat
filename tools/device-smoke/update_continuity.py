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
import subprocess
import sys
import time

import pairing_lifecycle as p
from session_cleanup import cleanup
from terminal_lifecycle import Terminal

PB = Path.home() / '.local/bin/pb'


def owners():
    out = subprocess.run(['pgrep', '-af', '__runtime-session-owner'], capture_output=True, text=True).stdout
    found = {}
    for line in out.splitlines():
        parts = line.split()
        if '--generation' in parts:
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
    ssh = subprocess.Popen([str(PB), 'ssh', target, '--', 'sh', '-c', 'echo PB_SSH_UP; sleep 600'],
                           stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
    try:
        deadline = time.monotonic() + 90
        up = ''
        while 'PB_SSH_UP' not in up:
            if time.monotonic() > deadline or ssh.poll() is not None:
                raise RuntimeError('the held pb ssh connection did not come up')
            if select.select([ssh.stdout], [], [], 1)[0]:
                up += ssh.stdout.readline()
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
        try:
            ssh.wait(timeout=15)
        except subprocess.TimeoutExpired:
            ssh.kill()
            ssh.wait(timeout=5)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('candidate')
    parser.add_argument('--hold', choices=['both', 'terminal', 'exec'], default='both')
    parser.add_argument('--exec-seconds', type=int, default=45)
    args = parser.parse_args()
    password = sys.stdin.readline().rstrip('\n')
    name = 'smoke-update-' + str(int(time.time()))
    before_version = p.invoke(PB, '--version', raw=True)
    terminal = Terminal([str(PB), 'connect', args.target, '--name', name, '--status-bar', 'off'])
    exec_process = None
    try:
        terminal.expect(b'\x1b[?2004h')
        terminal.expect(terminal.marker('READY', 'stty -echo; '))
        terminal.send("pb_hold=$$; printf '\\nPB_SHELL_%s\\n' \"$pb_hold\"\r")
        terminal.expect('PB_SHELL_')
        pid_line = terminal.expect('\n').decode().strip()
        held_pid = pid_line
        if not held_pid.isdigit():
            raise RuntimeError('could not read the held shell identity')
        held = set(owners())
        if not held:
            raise RuntimeError('no session owner is running for the held terminal')
        if args.hold != 'terminal':
            exec_process = subprocess.Popen([str(PB), 'exec', args.target, '--', 'sh', '-c',
                                             'sleep %d; echo EXEC_FINISHED' % args.exec_seconds], stdout=subprocess.PIPE,
                                            stderr=subprocess.PIPE, text=True)
        ssh_blocks_update(args.target, args.candidate, password)
        time.sleep(5)
        result = p.command_result(PB, 'update', '--from', args.candidate, '--json', timeout=420,
                                  terminal=True, password=password)
        if result.returncode:
            import re
            raise RuntimeError('update failed while work was held rc=%s: %s' % (result.returncode, re.sub(r'\x1b\[[0-9;?]*[a-zA-Z]', '', result.stdout)[-600:]))
        after_version = p.invoke(PB, '--version', raw=True)
        if after_version == before_version:
            raise RuntimeError('candidate must be a different build')
        # Without any user action the same shell answers.
        terminal.send("printf '\\nPB_AGAIN_%s\\n' \"$$\"\r")
        terminal.expect('PB_AGAIN_', timeout=180)
        again_pid = terminal.expect('\n', timeout=10).decode().strip()
        if again_pid != held_pid:
            raise RuntimeError('terminal reconnected to a different shell')
        if exec_process is not None:
            out, _ = exec_process.communicate(timeout=180)
            if exec_process.returncode != 0 or 'EXEC_FINISHED' not in out:
                raise RuntimeError('running exec did not finish normally across the update')
            exec_process = None
        current = owners()
        if not held & set(current):
            raise RuntimeError('old session owner vanished while its session was still open')
        terminal.send('exit\r')
        terminal.wait_exit(30)
        cleanup(str(PB), args.target, name)
        wait(lambda: not held & set(owners()), 180, 'old session owner retirement')
    finally:
        if exec_process is not None:
            exec_process.kill()
        terminal.close()
        try:
            cleanup(str(PB), args.target, name)
        except Exception:
            pass
    print(json.dumps({'check': 'update-continuity', 'target': args.target, 'status': 'PASS',
                      'same_shell_after_update': True, 'exec_finished': True,
                      'old_owner_retired': True, 'busy_while_ssh_open': True}), flush=True)


if __name__ == '__main__':
    main()
