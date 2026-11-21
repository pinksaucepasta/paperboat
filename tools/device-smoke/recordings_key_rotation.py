#!/usr/bin/env python3
"""C307, C308, F25-11, F25-12: replacing and resetting the team recording key, end to end on a team machine.

Usage: recordings_key_rotation.py TARGET --team TEAM [--vault-password-file FILE]
TARGET is a machine of TEAM that records. The signed-in account must be the team OWNER (reset is owner-only; replace
needs a copy of the current key) with an unlocked vault (--vault-password-file unlocks it first). Use a disposable team:
a key chain only grows, so the team ends two generations higher. The team needs no key beforehand: its first
recording creates generation 1 (asserted by key_created_by_first_recording).
Promises asserted (CHECKLIST F25-11, F25-12; cli/cmd/pb/recordings_command.go):
  replace: generation +1 with a copy for every viewer; older recordings STAY readable (sealed previous link); new ones work.
  reset:   generation +1 again, reported older_recordings_readable=false; recordings made before can be opened by no
           one (list/play/export of them fail, they stay listed); a recording made after works.
Every step runs; a failed step only skips the steps that need its result. One JSON line is printed: ids, generations and
exit codes only, never recording text, the password or a key.
"""
import argparse
import json
import os
import re
import sys
import time

import winhost
from pairing_lifecycle import command_result
import recordings_lifecycle as rec

PB = winhost.pb_path()


def key_command(action, team):
    """`pb recordings key replace|reset`: the new generation and the copies handed out."""
    result = command_result(PB, 'recordings', 'key', action, '--team', team, '--confirm', team, '--json', timeout=180)
    if result.returncode:
        raise RuntimeError('pb recordings key %s failed: exit %d' % (action, result.returncode))  # no output: it may name a key
    receipt = json.loads(result.stdout)
    if receipt.get('team') != team or receipt.get('copies', 0) < 1 or receipt.get('generation', 0) < 2:
        raise RuntimeError('pb recordings key %s did not report a new generation with copies' % action)
    return receipt


def key_generation(team):
    """The team's recording key generation through `pb recordings key status` (0: no key). No other CLI command shows it."""
    listed = command_result(PB, 'recordings', 'key', '--help', timeout=60)
    if listed.returncode or not re.search(r'^\s+status\b', listed.stdout + listed.stderr, re.M):
        raise RuntimeError('cannot read recording key generation: pb recordings key has no status command')
    result = command_result(PB, 'recordings', 'key', 'status', '--team', team, '--json', timeout=90)
    if result.returncode:
        raise RuntimeError('cannot read recording key generation: pb recordings key status failed: exit %d' % result.returncode)
    try:
        receipt = json.loads(result.stdout)
        return int((receipt.get('data') or receipt).get('generation', 0))
    except (ValueError, TypeError, AttributeError):
        raise RuntimeError('cannot read recording key generation: pb recordings key status did not answer JSON') from None


def readable(recording_id, directory):
    rec.export_text(recording_id, directory)
    rec.played_text(recording_id)


def refused(recording_id, directory):
    """Both replay forms fail for a recording whose key is gone; the exit codes are the evidence."""
    codes = {}
    for name, args in (('export', ['export', recording_id, '--output', str(directory / ('gone-' + recording_id + '.cast'))]),
                       ('play', ['play', recording_id, '--idle-limit', '1s', '--speed', '8'])):
        codes[name] = command_result(PB, 'recordings', *args, timeout=180).returncode
        if not codes[name]:
            raise RuntimeError('pb recordings %s opened a recording whose key was reset' % name)
    return codes


def run(team, target, directory, steps=None):
    steps = [] if steps is None else steps
    state = {}

    def step(name, depends, action):
        if any(d not in state for d in depends):
            steps.append({'step': name, 'status': 'SKIP', 'reason': 'needs ' + ', '.join(d for d in depends if d not in state)})
            return
        began = time.monotonic()
        try:
            state[name] = action() or True
            steps.append({'step': name, 'status': 'PASS', 'seconds': round(time.monotonic() - began, 1)})
        except Exception as error:  # no fail-fast: the remaining independent steps still run
            steps.append({'step': name, 'status': 'FAIL', 'error': str(error)[:300]})

    def replaced():
        receipt = key_command('replace', team)
        state['replace_generation'] = receipt['generation']
        return receipt

    def reset():
        receipt = key_command('reset', team)
        if receipt.get('older_recordings_readable') is not False:
            raise RuntimeError('reset did not say older recordings are unreadable')
        if receipt['generation'] != state['replace']['generation'] + 1:
            raise RuntimeError('reset did not make generation %d' % (state['replace']['generation'] + 1))
        return receipt

    def first_recording_made_the_key():
        if key_generation(team) < 1:  # the team's first recording makes generation 1; nobody creates it by hand
            raise RuntimeError("the team's first recording did not create recording key generation 1")

    def listed():
        if not {state['record_old'], state['record_new']} <= {r['id'] for r in rec.listing(team)}:
            raise RuntimeError('a recording vanished from the list')

    step('record_old', [], lambda: rec.phase_manual(team, target, directory))
    step('key_created_by_first_recording', ['record_old'], first_recording_made_the_key)
    step('replace', ['record_old'], replaced)
    step('old_readable_after_replace', ['replace'], lambda: readable(state['record_old'], directory))
    step('record_new', ['replace'], lambda: rec.phase_manual(team, target, directory))
    step('reset', ['replace'], reset)
    step('old_unreadable_after_reset', ['reset'], lambda: refused(state['record_old'], directory))
    step('new_unreadable_after_reset', ['reset', 'record_new'], lambda: refused(state['record_new'], directory))
    step('listed_after_reset', ['reset', 'record_new'], listed)
    step('record_after_reset', ['reset'], lambda: rec.phase_manual(team, target, directory))
    return {'check': 'recording-key-rotation', 'target': target,
            'status': 'PASS' if all(s['status'] == 'PASS' for s in steps) else 'FAIL', 'steps': steps,
            'recordings': {k: state[k] for k in ('record_old', 'record_new', 'record_after_reset') if isinstance(state.get(k), str)},
            'generations': {'replace': state.get('replace_generation'), 'reset': (state.get('reset') or {}).get('generation')}}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--team', required=True)
    parser.add_argument('--vault-password-file')
    args = parser.parse_args()
    os.environ['PB_SMOKE_WORKSPACE'] = args.team
    rec.unlock(args.vault_password_file)
    with winhost.workdir('paperboat-keyrot-') as directory:  # removed on every exit path
        receipt = run(args.team, args.target, directory)
    print(json.dumps(receipt), flush=True)
    sys.exit(0 if receipt['status'] == 'PASS' else 1)


if __name__ == '__main__':
    main()
