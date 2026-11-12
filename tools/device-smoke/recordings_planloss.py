#!/usr/bin/env python3
"""F25-03: a team that loses its paid plan stops every recording; recordings made before stay.

Usage: recordings_planloss.py TARGET --team TEAM [--vault-password-file FILE] [--wait-minutes 40]
TARGET is a machine of TEAM (any OS) that records. The check starts a recording in an attached session, prints one JSON line
{"armed": true} and waits for someone to end the team's paid plan (test data: the operator revokes the entitlement). Then it
requires: a new start is refused and, once the plan is back, the recording is listed as ended at the moment of the loss (no stop was sent), the team's recordings are
refused with team_plan_required. It then waits for the plan to come back and requires the recording to be listed as ended
with its segments. Nothing but ids and this check's own marker tokens is printed.
"""
import argparse
import datetime
import json
import os
import sys
import time

import winhost
from pairing_lifecycle import command_result
import recordings_lifecycle as rec

PB = winhost.pb_path()


def plan_lost(team):
    result = command_result(PB, 'recordings', 'list', '--team', team, '--json', timeout=90)
    return result.returncode != 0 and 'team_plan_required' in (result.stdout or '') + (result.stderr or '')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--team', required=True)
    parser.add_argument('--vault-password-file')
    parser.add_argument('--wait-minutes', type=int, default=40)
    args = parser.parse_args()
    os.environ['PB_SMOKE_WORKSPACE'] = args.team
    rec.unlock(args.vault_password_file)
    wait = args.wait_minutes * 60
    before = {r['id'] for r in rec.listing(args.team)}
    name = 'smoke-rec-' + os.urandom(5).hex()
    session = rec.Session(args.target, name)
    receipt = {'check': 'recordings-plan-loss', 'target': args.target, 'status': 'PASS', 'phases': []}
    try:
        sid = session.session_id()
        rec.recording_control('start', sid)
        rec.wait_for('REC in this client', session.saw_rec, timeout=30, interval=0.3)
        session.say('PLANON')
        recording = rec.new_recording(args.team, args.target, before)
        armed_at = time.time()
        print(json.dumps({'armed': True, 'recording': recording['id']}), flush=True)
        rec.wait_for('the team to lose its plan', lambda: plan_lost(args.team), timeout=wait, interval=10)
        lost_at = time.time()
        refused = command_result(PB, 'recordings', 'start', sid, '--json', timeout=90)
        if refused.returncode == 0:
            raise RuntimeError('a recording started after the team lost its plan')
        receipt['phases'].append('new_start_refused')
        session.say('PLANOFF')
        rec.wait_for('the plan to come back', lambda: not plan_lost(args.team), timeout=wait, interval=10)
        ended = rec.wait_for('the recording to be listed as ended', lambda: next((r for r in rec.listing(args.team) if r['id'] == recording['id'] and r['state'] == 'ended'), None), timeout=600, interval=10)
        if ended['segments'] < 1:
            raise RuntimeError('the recording made before the plan was lost is gone')
        # Nobody sent a stop and the session lives on: the recording ended when the plan went, not when the check ended it.
        ended_at = datetime.datetime.fromisoformat(ended['ended_at'].replace('Z', '+00:00')).timestamp()
        if not armed_at <= ended_at <= lost_at + 30:
            raise RuntimeError('the recording did not end when the team lost its plan (ended %.0f s from the loss)' % (ended_at - lost_at))
        receipt['phases'] += ['recording_stopped_without_a_stop', 'recording_kept']
        receipt['seconds_from_loss_to_stop'] = round(ended_at - lost_at, 1)
    finally:
        session.end()
        rec.cleanup(PB, args.target, name)
    print(json.dumps(receipt), flush=True)


if __name__ == '__main__':
    main()
