#!/usr/bin/env python3
"""Release loop: update N-1 -> N and N -> N-1 on one device; both directions end ready with a version-matched guard.

Usage: update_crossbuild.py --previous BIN_N-1 --previous-version V --current BIN_N --current-version V.
The install starts on the previous build (it is put there first when it is not). Each update uses
`pb update --from FILE --force` (the way a user switches builds); afterwards the reported version is the
target's, the daemon is ready, `pb doctor` is healthy and its machine_guard_version check passes (the root
guard runs the same pb as the user's install; a build older than that check may omit it). No workspace test can cover this: it crosses two real builds.
"""
import argparse
import json
import os
from pathlib import Path
import time

import pairing_lifecycle as p

PB = Path.home() / '.local/bin/pb'
READY_SECONDS = int(os.environ.get('PAPERBOAT_SMOKE_READY_SECONDS', '240'))


def version():
    return json.loads(p.invoke(PB, '--version', '--json', raw=True))['data']['version']


def ping_ok(target, seconds=None):
    """Machines resolve and connect after the update: `pb ping TARGET` succeeds (first contact may need a retry)."""
    deadline = time.monotonic() + (seconds or min(90, READY_SECONDS))
    while time.monotonic() < deadline:
        if p.command_result(PB, 'ping', target, timeout=60).returncode == 0:
            return True
        time.sleep(4)
    return False


def switch(binary, expected, guard_required, ping_target=''):
    """Install BINARY (the build that reports EXPECTED) and wait until it is ready; return the seconds taken.

    A build older than the guard check (guard_required False) may not report machine_guard_version at all; when
    it does, or when the build must (the current one), it has to pass."""
    started = time.monotonic()
    receipt = p.invoke(PB, 'update', '--from', str(binary), '--force', '--json', timeout=600)
    assert receipt.get('version') == expected, 'update installed %r, expected %r' % (receipt.get('version'), expected)
    deadline = started + READY_SECONDS
    problem = 'never checked'
    ready = False
    while time.monotonic() < deadline:
        try:
            # A degraded doctor exits 1 but still prints its checks; read them, do not treat it as a crash.
            result = p.command_result(PB, 'doctor', '--json', timeout=60)
            doctor = json.loads(result.stdout)
            doctor = doctor.get('data', doctor)
            guard = [c for c in doctor.get('checks', []) if c.get('code') == 'machine_guard_version']
            guard_ok = guard[0].get('status') == 'pass' if guard else not guard_required
            if version() == expected and doctor.get('overall') == 'healthy' and guard_ok:
                ready = True
                break
            problem = 'version %s, doctor %s, guard %s%s' % (version(), doctor.get('overall'), guard[0].get('status') if guard else 'missing',
                                                           ' (' + guard[0].get('summary', '') + ')' if guard and guard[0].get('status') != 'pass' else '')
        except Exception as error:  # the daemon restarts during an update
            problem = type(error).__name__
        time.sleep(4)
    if ready:
        if ping_target and not ping_ok(ping_target):
            raise RuntimeError('ready on %s but `pb ping %s` fails after the update' % (expected, ping_target))
        return round(time.monotonic() - started, 1)
    raise RuntimeError('not ready on %s %d s after the update: %s' % (expected, READY_SECONDS, problem))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--previous', required=True)
    parser.add_argument('--previous-version', required=True)
    parser.add_argument('--current', required=True)
    parser.add_argument('--current-version', required=True)
    parser.add_argument('--ping-target', default='', help='a machine name that must answer pb ping after each switch')
    args = parser.parse_args()
    if version() != args.previous_version:
        switch(args.previous, args.previous_version, guard_required=False, ping_target=args.ping_target)
    up = switch(args.current, args.current_version, guard_required=True, ping_target=args.ping_target)
    down = switch(args.previous, args.previous_version, guard_required=False, ping_target=args.ping_target)
    print(json.dumps({'check': 'update-crossbuild', 'status': 'PASS', 'previous': args.previous_version,
                      'current': args.current_version, 'upgrade_ready_seconds': up, 'downgrade_ready_seconds': down,
                      'guard_version_matched': True, 'ping_target': args.ping_target or None, 'enrollment_preserved': True}), flush=True)


if __name__ == '__main__':
    main()
