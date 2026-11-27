#!/usr/bin/env python3
"""The recordings console-recording-replay (console_rows.py, F25-05) replays in the browser, made on a team machine.

Usage: record_for_replay.py TARGET --team TEAM --vault-password-file FILE --console-start-cmd CMD
Runs where recordings_key_rotation.py runs (the team owner's pb); every start goes through the console (CMD, as there).
`normal` prints a line a second for TICKS seconds (play, pause and seek need a clock that moves), `tiny` is a few
keystrokes, `long` is tens of MB, so the replay is still downloading when the replay rule is removed.
Prints one JSON line {"status", "recordings": {name: id}, "errors": [...]}: ids only, never terminal text.
"""
import argparse
import json
import os
import sys
import time
import uuid

import winhost
import recordings_key_rotation as rotation
import recordings_lifecycle as rec
from session_cleanup import cleanup

TICKS = 12
LONG_BYTES = 30_000_000  # about 40 MB of base64 output


def record(team, target, during):
    """One manual recording around during(session); returns its id once the server lists it as ended."""
    before = {r['id'] for r in rec.listing(team)}
    name = 'smoke-rec-' + uuid.uuid4().hex[:10]
    session = rec.Session(target, name)
    try:
        sid = session.session_id()
        rec.recording_control('start', sid)
        rec.wait_for('REC in this client', session.saw_rec, timeout=30, interval=0.3)
        during(session)
        rec.recording_control('stop', sid)
        recording = rec.new_recording(team, target, before)
        rec.finished(team, recording['id'])
        return recording['id']
    finally:
        session.end()
        cleanup(rec.PB, target, name)


def ticking(session):
    for i in range(TICKS):
        session.say('TICK%d' % i)
        time.sleep(1)


def run(team, target, directory, console_cmd):
    real, rec.recording_control = rotation.console_start(team, console_cmd)
    made, errors = {}, []
    try:
        for name, action in (('normal', lambda: record(team, target, ticking)),
                             ('tiny', lambda: record(team, target, lambda session: session.say('TINY'))),
                             ('long', lambda: rec.phase_long(team, target, directory, replay=False, size=LONG_BYTES))):
            try:
                made[name] = action()
            except Exception as error:  # noqa: BLE001 no fail-fast: the other recordings are still made and replayed
                errors.append('%s: %s' % (name, str(error)[:200]))
    finally:
        rec.recording_control = real
    return {'check': 'record-for-replay', 'status': 'PASS' if not errors else 'FAIL', 'recordings': made, 'errors': errors}


def main():
    parser = argparse.ArgumentParser(description=__doc__.split('\n')[0])
    parser.add_argument('target')
    parser.add_argument('--team', required=True)
    parser.add_argument('--vault-password-file', required=True)
    parser.add_argument('--console-start-cmd', required=True)
    args = parser.parse_args()
    os.environ['PB_SMOKE_WORKSPACE'] = args.team
    rec.unlock(args.vault_password_file)
    with winhost.workdir('paperboat-replay-') as directory:
        receipt = run(args.team, args.target, directory, args.console_start_cmd)
    print(json.dumps(receipt), flush=True)
    sys.exit(0 if receipt['status'] == 'PASS' else 1)


if __name__ == '__main__':
    main()
