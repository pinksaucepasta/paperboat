#!/usr/bin/env python3
"""F10-05: a revoke never withholds ENV from the others; the owner's explicit rotation re-seals and host show tells the truth.

  env_revoke_rotation.py SURVIVOR --revoke MACHINE_ID --password-file ABSOLUTE_OWNER_ONLY_FILE

Run on a machine with an unlocked ENV vault. SURVIVOR is another machine of the same account that
stays; MACHINE_ID is a throwaway machine of the account that is revoked (never a shared machine).
  1. a personal variable is set and delivered to SURVIVOR (host show: published and applied)
  2. MACHINE_ID is revoked: SURVIVOR's delivery stays published and applied at the same fence, a command
     there sees the value and says nothing about ENV (FIX-ENVRESEAL bbb8f2bb2: no pending state, no unlock)
  3. `pb env rotate` (the owner's explicit rotation) re-seals SURVIVOR at a newer fence, applied, value still seen
  4. the revoked machine is not given the rotated delivery (host show: nothing published at the new fence)
"""
import argparse
import json
from pathlib import Path
import subprocess
import time
import uuid

from pairing_lifecycle import confirmed_command
import windows_target
import winhost

PB = winhost.pb_path()
WAIT_SECONDS = 180


def run(*args, stdin=None, timeout=90):
    result = winhost.pb_text(PB, args, input=stdin, timeout=timeout)
    return result.returncode, result.stdout, result.stderr


def host(machine):
    code, out, _ = run('env', 'host', 'show', '--machine', machine, '--json')
    return code, (json.loads(out) if code == 0 else None)


def launch(machine):
    # The host reports an ENV delivery as applied when a process is launched with it.
    run('exec', machine, '--', *windows_target.true_argv(PB, machine))


def fence(show):
    return max([layer['fence_generation'] for layer in show['layers']] or [0])


def sees(machine, name):
    code, out, err = run('exec', machine, '--', *windows_target.target_argv(
        PB, machine, ['sh', '-c', 'echo "[$%s]"' % name], windows_target.bracket_env_script(name)))
    if code or '[rotation-value]' not in out or 'ENV not applied' in err:
        raise RuntimeError('the surviving machine does not see the value or says ENV was not applied')


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
    result = {'check': 'env-revoke-rotation', 'status': 'PASS', 'survivor_untouched_by_revoke': True}
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
        # A removal never withholds ENV from the others (bbb8f2bb2): the delivery stays published and applied at the same fence.
        time.sleep(10)
        launch(args.survivor)
        code, show = host(args.survivor)
        if code or not (show['published'] and show['applied']) or fence(show) != before:
            raise RuntimeError('the revoke changed the surviving machine delivery: %s' % (
                json.dumps({'published': show['published'], 'applied': show['applied'], 'fence': fence(show), 'before': before}) if code == 0 else 'unreadable'))
        sees(args.survivor, name)
        # The owner's explicit rotation re-seals for the survivors at a newer fence; the revoked machine is not a recipient.
        code, _, err = run('env', 'unlock', '--password-file', args.password_file, '--json')
        if code:
            raise RuntimeError('unlocking the vault failed')
        code, _, err = run('env', 'rotate', '--json', timeout=180)
        if code:
            raise RuntimeError('pb env rotate failed: ' + err[:120])
        deadline = time.monotonic() + WAIT_SECONDS
        while True:
            launch(args.survivor)
            code, show = host(args.survivor)
            if code == 0 and show['published'] and show['applied'] and fence(show) > before:
                break
            if time.monotonic() > deadline:
                raise RuntimeError('delivery was not re-sealed after the rotation: before=%s now=%s' % (
                    before, json.dumps({'published': show['published'], 'applied': show['applied'], 'fence': fence(show)}) if code == 0 else 'unreadable'))
            time.sleep(3)
        sees(args.survivor, name)
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
