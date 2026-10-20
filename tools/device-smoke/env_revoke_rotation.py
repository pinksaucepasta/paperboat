#!/usr/bin/env python3
"""F10-05: after a machine is revoked, ENV is re-sealed for the others and host show tells the truth.

  env_revoke_rotation.py SURVIVOR --revoke MACHINE_ID --password-file ABSOLUTE_OWNER_ONLY_FILE

Run on a machine with an unlocked ENV vault. SURVIVOR is another machine of the same account that
stays; MACHINE_ID is a throwaway machine of the account that is revoked (never a shared machine).
  1. a personal variable is set and delivered to SURVIVOR (host show: published and applied)
  2. MACHINE_ID is revoked
  3. SURVIVOR's delivery turns pending (host show says not published, not applied) and a command
     there still runs and says so; unlocking the vault from this machine (the documented recovery)
     re-seals it at a newer fence generation, it is applied again, and a new command on SURVIVOR
     sees the value
  4. the revoked machine is not given the rotated delivery (host show: nothing published at the new fence)
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
    code, out, _ = run('env', 'host', 'show', '--machine', machine, '--json')
    return code, (json.loads(out) if code == 0 else None)


def launch(machine):
    # The host reports an ENV delivery as applied when a process is launched with it.
    run('exec', machine, '--', 'true')


def fence(show):
    return max([layer['fence_generation'] for layer in show['layers']] or [0])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('survivor')
    parser.add_argument('--revoke', required=True, dest='revoked')
    parser.add_argument('--password-file', required=True, help='absolute owner-only file with the ENV vault password')
    args = parser.parse_args()
    name = 'PB_SMOKE_' + uuid.uuid4().hex[:8].upper()
    code, _, err = run('env', 'set', name, '--value-stdin', '--json', stdin='rotation-value')
    if code:
        raise RuntimeError('could not set the probe variable: ' + err[:120])
    result = {'check': 'env-revoke-rotation', 'status': 'PASS', 'pending_observed': False}
    try:
        deadline = time.monotonic() + WAIT_SECONDS
        while True:
            launch(args.survivor)
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
            launch(args.survivor)
            code, show = host(args.survivor)
            if code == 0 and not show['published'] and not show['applied'] and fence(show) > before:
                result['pending_observed'] = True
                break
            if time.monotonic() > deadline:
                raise RuntimeError('the surviving machine never showed a pending delivery after the revoke: %s' % (
                    json.dumps({'published': show['published'], 'applied': show['applied'], 'fence': fence(show)}) if code == 0 else 'unreadable'))
            time.sleep(3)
        code, out, err = run('exec', args.survivor, '--', 'true')
        if code or 'ENV not applied' not in err:
            raise RuntimeError('a command on the pending machine did not run and say that ENV was not applied')
        code, _, _ = run('env', 'vault', 'unlock', '--password-file', args.password_file, '--json')
        if code:
            raise RuntimeError('unlocking the vault failed')
        deadline = time.monotonic() + WAIT_SECONDS
        while True:
            launch(args.survivor)
            code, show = host(args.survivor)
            if code == 0 and show['published'] and show['applied'] and fence(show) > before:
                break
            if time.monotonic() > deadline:
                raise RuntimeError('delivery was not re-sealed after unlocking: before=%s now=%s' % (
                    before, json.dumps({'published': show['published'], 'applied': show['applied'], 'fence': fence(show)}) if code == 0 else 'unreadable'))
            time.sleep(3)
        code, out, _ = run('exec', args.survivor, '--', 'sh', '-c', 'echo "[$%s]"' % name)
        if code or '[rotation-value]' not in out:
            raise RuntimeError('the surviving machine does not see the value after rotation')
        code, revoked = host(args.revoked)
        if code == 0 and any(layer['published'] and layer['fence_generation'] >= fence(show) for layer in revoked['layers']):
            raise RuntimeError('the revoked machine was given the rotated delivery: %s' % json.dumps(
                [(layer['state'], layer['fence_generation']) for layer in revoked['layers']]))
        result['fence_before'], result['fence_after'] = before, fence(show)
    finally:
        challenge = json.loads(run('env', 'unset', name, '--json')[1] or '{}')
        token = challenge.get('data', {}).get('confirmation_token')
        if token:
            run('env', 'unset', name, '--json', '--confirm', token)
    print(json.dumps(result), flush=True)


if __name__ == '__main__':
    main()
