#!/usr/bin/env python3
"""exec and ssh to this machine itself and to a peer must work within 5 s of a daemon restart.

The local daemon is restarted (`pb service restart` returns once it is back); then, for the machine itself and for the
peer, `pb exec` and `pb ssh` (one after the other, each within DEADLINE seconds, the first right after the restart) must each print a product the REMOTE side computed (the arithmetic is in the command's
argument, never its text), for up to DEADLINE seconds. Every combination runs and all failures are reported together:
the Windows exec-to-self wedge and the Mac readiness flap after a restart both hid behind a passing first command."""
import argparse
import random
import subprocess
import time

from collect import run_all
from install_lifecycle import registration
from pairing_lifecycle import invoke
import windows_target
import winhost

DEADLINE = 5.0


def probe(binary, target, command='exec'):
    """(argv tail, expected stdout line): a sum no argument contains, computed by the remote shell. `pb ssh` joins its
    arguments with spaces for the remote login shell, so there the command is ONE argument (`sh -c a b` would run `a`)."""
    x, y = random.randint(1000, 9999), random.randint(1000, 9999)
    if windows_target.is_windows(binary, target):
        return ['cmd', '/c', 'set', '/a', '%d+%d' % (x, y)], str(x + y)
    if command == 'ssh':
        return ['echo $((%d+%d))' % (x, y)], str(x + y)
    return ['sh', '-c', 'echo $((%d+%d))' % (x, y)], str(x + y)


def verdict(elapsed, returncode, output, expected, deadline=DEADLINE):
    """PASS iff it succeeded in time and the remote-computed line is alone on a line of the output."""
    if elapsed > deadline:
        return False, 'took %.1fs, over the %.0fs deadline' % (elapsed, deadline)
    if returncode != 0:
        return False, 'exit %s' % returncode
    if expected not in output.replace('\r', '').split('\n'):
        return False, 'the remote answer %s was not in the output' % expected
    return True, ''


def attempt(binary, command, target, argv, expected, started, now=time.monotonic, deadline=DEADLINE):
    """Retry `pb <command> <target> -- argv` until the deadline from `started`; the last failure names why."""
    reason = 'no attempt'
    while True:
        remaining = deadline - (now() - started)
        if remaining <= 0:
            raise RuntimeError('%s %s: still failing %.0fs after the restart: %s' % (command, target, deadline, reason))
        try:
            result = winhost.run_pb(binary, [command, target, '--', *argv], max(1, remaining))
            ok, reason = verdict(now() - started, result.returncode, result.stdout.decode(errors='replace'), expected, deadline)
            if ok:
                return
            reason += ': ' + result.stderr.decode(errors='replace').strip()[-120:]
        except subprocess.TimeoutExpired:
            reason = 'no answer within %.0fs' % remaining
        time.sleep(0.25)


def restart_local(binary):
    result = winhost.run_pb(binary, ['service', 'restart'], 120)
    if result.returncode:
        raise RuntimeError('pb service restart failed: ' + result.stderr.decode(errors='replace')[-200:])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('peer')
    parser.add_argument('--self', dest='own', help='this machine\'s alias (skips the machine list lookup: 128 machines take minutes on Windows)')
    args = parser.parse_args()
    binary = winhost.pb_path()
    own = args.own or next(m['alias'] for m in invoke(binary, 'machine', 'list', '--all', '--json')['machines']
                           if m['id'] == registration()['machine_id'])
    restart_local(binary)
    steps = []
    for label, target in (('self', own), ('peer', args.peer)):
        for command in ('exec', 'ssh'):
            argv, expected = probe(binary, target, command)
            steps.append(('%s %s' % (command, label), lambda c=command, t=target, a=argv, e=expected: attempt(binary, c, t, a, e, time.monotonic())))
    # One after the other, each with its own 5 s from its own start: the first starts the moment the restart returned, so
    # a blackout after the restart fails it; four at once contend for the same connections and are slower than any one.
    run_all(*steps)
    print('{"check": "exec-ssh-after-restart", "status": "PASS", "deadline_s": %d}' % DEADLINE)


if __name__ == '__main__':
    main()
