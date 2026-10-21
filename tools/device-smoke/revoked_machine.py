#!/usr/bin/env python3
"""F2-05: a revoked machine loses access immediately, says so, and does not hammer the server.

  revoked_machine.py revoke MACHINE_ID          on another machine of the account: revoke it (exact id)
  revoked_machine.py verify PEER [--window 120] on the revoked machine, right after the revoke:
    1. a command to PEER fails at once and says the session is no longer valid and to run pb login
    2. pb status says the same
    3. after a settle period the daemon's diagnostic events grow slowly. Several background loops
       (usage, observations, trust, events) each fail about every 10 s, and each failure is one or two
       events, so a healthy signed-out daemon stays under ~45 a minute; a loop that retries every
       second or two (the bug this found: ~75 a minute) does not
"""
import argparse
import json
from pathlib import Path
import subprocess
import time

PB = Path.home() / '.local/bin/pb'
SIGNED_OUT = 'no longer valid'


def run(*args, timeout=60):
    result = subprocess.run([str(PB), *args], capture_output=True, text=True, timeout=timeout)
    return result.returncode, result.stdout, result.stderr


def events():
    total = 0
    for path in (Path.home() / '.local/state/paperboat/diagnostics').glob('events-*.ndjson'):
        total += sum(1 for _ in path.open('rb'))
    return total


def revoke(machine_id):
    from pairing_lifecycle import confirmed_command
    result = confirmed_command(PB, 'machine', 'revoke', machine_id)
    if result.get('outcome') != 'confirmed' or result['machine']['id'] != machine_id:
        raise RuntimeError('revoke receipt did not identify the exact machine')
    print(json.dumps({'check': 'revoked-machine', 'phase': 'revoke', 'machine': machine_id, 'status': 'PASS'}), flush=True)


def verify(peer, settle, window, limit_per_minute):
    started = time.monotonic()
    code, _, err = run('exec', peer, '--', 'true')
    first = time.monotonic() - started
    if code == 0 or SIGNED_OUT not in err or 'pb login' not in err:
        raise RuntimeError('exec on a revoked machine does not say the session is no longer valid: rc=%d %s' % (code, err[:160]))
    code, _, err = run('status')
    if SIGNED_OUT not in err or 'pb login' not in err:
        raise RuntimeError('pb status on a revoked machine does not say the session is no longer valid: %s' % err[:160])
    time.sleep(settle)
    before = events()
    time.sleep(window)
    rate = (events() - before) * 60.0 / window
    if rate > limit_per_minute:
        raise RuntimeError('the revoked machine keeps retrying: %.1f diagnostic events a minute (limit %d)' % (rate, limit_per_minute))
    print(json.dumps({'check': 'revoked-machine', 'phase': 'verify', 'status': 'PASS', 'exec_refused_in_s': round(first, 1),
                      'events_per_minute': round(rate, 1)}), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('phase', choices=['revoke', 'verify'])
    parser.add_argument('target')
    parser.add_argument('--settle', type=int, default=90)
    parser.add_argument('--window', type=int, default=120)
    parser.add_argument('--limit-per-minute', type=int, default=45)
    args = parser.parse_args()
    if args.phase == 'revoke':
        revoke(args.target)
    else:
        verify(args.target, args.settle, args.window, args.limit_per_minute)


if __name__ == '__main__':
    main()
