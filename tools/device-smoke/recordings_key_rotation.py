#!/usr/bin/env python3
"""C307, C308, F25-11, F25-12: replacing and resetting the team recording key, end to end on a team machine.

Usage: recordings_key_rotation.py TARGET --team TEAM [--vault-password-file FILE] --member-cmd 'PB-OF-A-TEAM-MEMBER'
TARGET is a machine of TEAM that records. The signed-in account must be the team OWNER (reset is owner-only; replace
needs a copy of the current key) with an unlocked vault (--vault-password-file unlocks it first). Use a disposable team:
a key chain only grows, so the team ends two generations higher. The team must have NO recording key beforehand: its
first recording creates generation 1 (asserted by key_created_by_first_recording). --member-cmd is a command that runs the
pb of a plain team member who can replay (arguments are appended, e.g. 'ssh member-host .local/bin/pb'); the member's vault
must already be unlocked. Without it the member steps FAIL (never skip): the key must open for more than its owner.
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
import shlex
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


def member_pb(member_cmd, *args):
    if not member_cmd:
        raise RuntimeError('SMOKE_RECKEY_MEMBER_CMD is not set: a team member must replay the recordings too')
    argv = shlex.split(member_cmd)
    return command_result(argv[0], *argv[1:], *args, timeout=180)


def member_reads(member_cmd, recording_id):
    if member_pb(member_cmd, 'recordings', 'play', recording_id, '--idle-limit', '1s', '--speed', '8').returncode:
        raise RuntimeError('a team member cannot replay %s' % recording_id)


def member_refused(member_cmd, recording_id):
    if not member_pb(member_cmd, 'recordings', 'play', recording_id, '--idle-limit', '1s', '--speed', '8').returncode:
        raise RuntimeError('a team member replayed %s after the key was reset' % recording_id)


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


def run(team, target, directory, steps=None, member_cmd=None):
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
        first = state.get('key_created_by_first_recording')
        if isinstance(first, int) and not isinstance(first, bool) and receipt['generation'] != first + 1:
            raise RuntimeError('replace made generation %d, not %d' % (receipt['generation'], first + 1))
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
        generation = key_generation(team)  # the team's first recording makes generation 1; nobody creates it by hand
        if generation < 1:
            raise RuntimeError("the team's first recording did not create recording key generation 1")
        if generation != 1:
            raise RuntimeError('the team already had recording key generation %d: use a team with no key' % generation)
        return generation

    def listed():
        if not {state['record_old'], state['record_new']} <= {r['id'] for r in rec.listing(team)}:
            raise RuntimeError('a recording vanished from the list')

    step('record_old', [], lambda: rec.phase_manual(team, target, directory))
    step('key_created_by_first_recording', ['record_old'], first_recording_made_the_key)
    step('member_reads_first_recording', ['record_old'], lambda: member_reads(member_cmd, state['record_old']))
    step('replace', ['record_old'], replaced)
    step('old_readable_after_replace', ['replace'], lambda: readable(state['record_old'], directory))
    step('member_reads_old_after_replace', ['replace'], lambda: member_reads(member_cmd, state['record_old']))
    step('record_new', ['replace'], lambda: rec.phase_manual(team, target, directory))
    step('member_reads_new_after_replace', ['record_new'], lambda: member_reads(member_cmd, state['record_new']))
    step('reset', ['replace'], reset)
    step('old_unreadable_after_reset', ['reset'], lambda: refused(state['record_old'], directory))
    step('new_unreadable_after_reset', ['reset', 'record_new'], lambda: refused(state['record_new'], directory))
    step('member_refused_old_after_reset', ['reset'], lambda: member_refused(member_cmd, state['record_old']))
    step('member_refused_new_after_reset', ['reset', 'record_new'], lambda: member_refused(member_cmd, state['record_new']))
    step('listed_after_reset', ['reset', 'record_new'], listed)
    step('record_after_reset', ['reset'], lambda: rec.phase_manual(team, target, directory))
    step('member_reads_after_reset', ['record_after_reset'], lambda: member_reads(member_cmd, state['record_after_reset']))
    return {'check': 'recording-key-rotation', 'target': target,
            'status': 'PASS' if all(s['status'] == 'PASS' for s in steps) else 'FAIL', 'steps': steps,
            'recordings': {k: state[k] for k in ('record_old', 'record_new', 'record_after_reset') if isinstance(state.get(k), str)},
            'generations': {'first': state.get('key_created_by_first_recording') if isinstance(state.get('key_created_by_first_recording'), int) else None, 'replace': state.get('replace_generation'), 'reset': (state.get('reset') or {}).get('generation')}}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--team', required=True)
    parser.add_argument('--vault-password-file')
    parser.add_argument('--member-cmd', default='', help="runs a team member's pb; arguments are appended")
    args = parser.parse_args()
    os.environ['PB_SMOKE_WORKSPACE'] = args.team
    rec.unlock(args.vault_password_file)
    with winhost.workdir('paperboat-keyrot-') as directory:  # removed on every exit path
        receipt = run(args.team, args.target, directory, member_cmd=args.member_cmd)
    print(json.dumps(receipt), flush=True)
    sys.exit(0 if receipt['status'] == 'PASS' else 1)


if __name__ == '__main__':
    main()
