#!/usr/bin/env python3
"""F15-01: doctor is healthy only when terminal and exec work, explains a stopped daemon, and --repair fixes it."""
import argparse
import json
from pathlib import Path
import subprocess
import time
import uuid

import windows_target
import winhost

PB = winhost.pb_path()


def run(*args, timeout=90):
    result = winhost.pb_text(PB, args, timeout=timeout, stdin=subprocess.DEVNULL)
    return result.returncode, result.stdout, result.stderr


def doctor(*flags):
    code, out, _ = run('doctor', *flags, '--json')
    return json.loads(out)


def exec_works(target):
    return run('exec', target, '--', *windows_target.true_argv(PB, target), timeout=60)[0] == 0


def terminal_works(target):
    """A real PTY session starts, answers and ends cleanly (the same path a user takes)."""
    from terminal_lifecycle import Terminal
    from session_cleanup import cleanup
    name = 'smoke-doctor-' + str(int(time.time()))
    terminal = Terminal(winhost.terminal_argv(PB, ['connect', target, '--name', name, '--status-bar', 'off']), env=winhost.terminal_env(PB))
    try:
        if windows_target.is_windows(PB, target):  # cmd.exe under ConPTY: no bracketed paste, no stty, exit ends it
            token = 'PB_READY_' + uuid.uuid4().hex
            terminal.send(windows_target.typed_marker(token) + '\r')
            terminal.expect(token, timeout=60)
            terminal.send('exit\r')
        else:
            terminal.expect(b'\x1b[?2004h', timeout=60)
            terminal.expect(terminal.marker('READY', 'stty -echo; '), timeout=30)
            terminal.send(b'\x04')
        return terminal.wait_exit(15) == 0
    except Exception:
        return False
    finally:
        terminal.close()
        try:
            cleanup(PB if winhost.device() else str(PB), target, name)
        except Exception:
            pass


def wait_for(predicate, seconds, step):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(3)
    raise RuntimeError(step + ' did not happen within ' + str(seconds) + 's')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    healthy = doctor()
    if healthy['overall'] != 'healthy' or not exec_works(args.target) or not terminal_works(args.target):
        raise RuntimeError('machine must start healthy with working terminal and exec')
    if any(c['status'] != 'pass' for c in healthy['checks']):
        raise RuntimeError('healthy doctor reported a non-passing check')
    if run('service', 'stop')[0]:
        raise RuntimeError('could not stop the daemon service for the failure case')
    try:
        broken = doctor()
        failing = [c for c in broken['checks'] if c['status'] != 'pass']
        if broken['overall'] == 'healthy' or not failing:
            raise RuntimeError('doctor stayed healthy with the daemon stopped')
        if not all(c.get('recovery') or c.get('summary') for c in failing):
            raise RuntimeError('a failing check gave no explanation')
        code, out, err = run('doctor')
        if 'Recovery:' not in out + err:
            raise RuntimeError('human doctor output shows no recovery action')
    finally:
        run('doctor', '--repair')
    wait_for(lambda: doctor()['overall'] == 'healthy', 90, 'doctor --repair recovery')
    wait_for(lambda: exec_works(args.target), 90, 'exec after repair')
    wait_for(lambda: terminal_works(args.target), 90, 'terminal after repair')
    print(json.dumps({'check': 'doctor-lifecycle', 'target': args.target, 'status': 'PASS',
                      'healthy_requires_terminal_and_exec': True, 'failure_explained': True, 'repair_recovers': True}),
          flush=True)


if __name__ == '__main__':
    main()
