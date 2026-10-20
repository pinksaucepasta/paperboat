#!/usr/bin/env python3
"""F10-05: after a machine is revoked, ENV is re-sealed for the others and host show tells the truth.

  env_revoke_rotation.py SURVIVOR --revoke MACHINE_ID

Run on a machine with an unlocked ENV vault. SURVIVOR is another machine of the same account that
stays; MACHINE_ID is a throwaway machine of the account that is revoked (never a shared machine).
  1. a personal variable is set and delivered to SURVIVOR (host show: published and applied)
  2. MACHINE_ID is revoked
  3. SURVIVOR's delivery moves to a newer fence generation and is applied again, and a new command
     on SURVIVOR still sees the value; any pending state in between is allowed and recorded
  4. host show for the revoked machine no longer reports a delivery
"""
import argparse
import json
from pathlib import Path
import subprocess
import time
import uuid

from pairing_lifecycle import confirmed_command

PB = Path.home() / '.local/bin/pb'
WAIT_SECONDS = 180


def run(*args, stdin=None, timeout=90):
    result = subprocess.run([str(PB), *args], input=stdin, capture_output=True, text=True, timeout=timeout)
    return result.returncode, result.stdout, result.stderr


def host(machine):
    code, out, _ = run('env', 'host', 'show', machine, '--json')
    return code, (json.loads(out) if code == 0 else None)


def fence(show):
    return max([layer['fence_generation'] for layer in show['layers']] or [0])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('survivor')
    parser.add_argument('--revoke', required=True, dest='revoked')
    args = parser.parse_args()
    name = 'PB_SMOKE_' + uuid.uuid4().hex[:8].upper()
    code, _, err = run('env', 'set', name, '--value-stdin', '--json', stdin='rotation-value')
    if code:
        raise RuntimeError('could not set the probe variable: ' + err[:120])
    result = {'check': 'env-revoke-rotation', 'status': 'PASS', 'pending_observed': False}
    try:
        deadline = time.monotonic() + WAIT_SECONDS
        while True:
            code, show = host(args.survivor)
            if code == 0 and show['published'] and show['applied']:
                break
            if time.monotonic() > deadline:
                raise RuntimeError('delivery to the surviving machine never applied before the revoke')
            time.sleep(3)
        before = fence(show)
        confirmed_command(PB, 'machine', 'revoke', args.revoked)
        deadline = time.monotonic() + WAIT_SECONDS
        while True:
            code, show = host(args.survivor)
            if code == 0 and not show['applied']:
                result['pending_observed'] = True
            if code == 0 and show['published'] and show['applied'] and fence(show) > before:
                break
            if time.monotonic() > deadline:
                raise RuntimeError('delivery to the surviving machine was not re-sealed after the revoke')
            time.sleep(3)
        code, out, _ = run('exec', args.survivor, '--', 'sh', '-c', 'echo "[$%s]"' % name)
        if code or '[rotation-value]' not in out:
            raise RuntimeError('the surviving machine does not see the value after rotation')
        code, revoked = host(args.revoked)
        if code == 0 and revoked['layers']:
            raise RuntimeError('host show still reports a delivery to the revoked machine')
        result['fence_before'], result['fence_after'] = before, fence(show)
    finally:
        challenge = json.loads(run('env', 'unset', name, '--json')[1] or '{}')
        token = challenge.get('data', {}).get('confirmation_token')
        if token:
            run('env', 'unset', name, '--json', '--confirm', token)
    print(json.dumps(result), flush=True)


if __name__ == '__main__':
    main()
