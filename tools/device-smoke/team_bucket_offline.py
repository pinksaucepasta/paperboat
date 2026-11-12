#!/usr/bin/env python3
"""F25-07: a recording archived in the team bucket replays (CLI and browser) while its machine is offline.

Usage: team_bucket_offline.py TARGET --team TEAM --endpoint https://HOST --bucket NAME --s3-env FILE
           --stop-cmd CMD --start-cmd CMD [--browser-cmd CMD] [--region us-east-1] [--vault-password-file F]
Runs team_bucket_lifecycle's bucket phases first (save, refusals, a recording on TARGET archived as ciphertext, replay from the
bucket). Then it exports the archived recording, runs CMD --stop-cmd (a shell command here that stops Paperboat on TARGET),
waits until the server lists TARGET offline, exports the recording again from the bucket and requires the same bytes, runs
--browser-cmd (environment REC_ID, TEAM) while TARGET is offline, and runs --start-cmd and waits for TARGET online again before
the bucket is removed. Nothing but ids and counts is printed.
"""
import argparse
import json
import os
import subprocess
import sys
import time
import uuid

import winhost
from pairing_lifecycle import command_result
import recordings_lifecycle as rec
import team_bucket_lifecycle as tb

PB = winhost.pb_path()


def machine_state(team, target):
    result = command_result(PB, '--workspace', team, 'machine', 'list', '--all', '--json', timeout=90)
    if result.returncode:
        return None
    return next((m['state'] for m in json.loads(result.stdout)['machines'] if target in (m['alias'], m['id'])), None)


def shell(command, extra=None):
    done = subprocess.run(command, shell=True, timeout=900, env=dict(os.environ, **(extra or {})), capture_output=True, text=True)
    if done.returncode:
        raise RuntimeError('command failed (exit %d): %s' % (done.returncode, (done.stdout + done.stderr)[-1200:]))
    return done.stdout


def export_archive(recording, directory):
    path = directory / ('offline-' + uuid.uuid4().hex + '.cast')
    result = command_result(PB, 'recordings', 'export', recording, '--archive', '--output', str(path), timeout=300)
    if result.returncode:
        raise RuntimeError('pb recordings export --archive failed: exit %d: %s' % (result.returncode, (result.stderr or '')[-200:]))
    return path.read_bytes()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--team', required=True)
    parser.add_argument('--endpoint', required=True)
    parser.add_argument('--bucket', required=True)
    parser.add_argument('--region', default='us-east-1')
    parser.add_argument('--s3-env', required=True)
    parser.add_argument('--stop-cmd', required=True)
    parser.add_argument('--start-cmd', required=True)
    parser.add_argument('--browser-cmd')
    parser.add_argument('--vault-password-file')
    args = parser.parse_args()
    os.environ['PB_SMOKE_WORKSPACE'] = args.team
    values = dict(line.strip().split('=', 1) for line in open(args.s3_env) if '=' in line)
    access, secret = values['AK'], values['SK']
    rec.unlock(args.vault_password_file)
    started = time.monotonic()
    receipt = {'check': 'team-bucket-offline', 'target': args.target, 'status': 'PASS', 'phases': []}
    stopped = False
    try:
        prefix = tb.phase_commands(args, access, secret)
        receipt['phases'].append('bucket_get_set_refusals_keep_secret')
        # An unreachable bucket is named by its failing step and the saved bucket stays as it was.
        down = tb.bucket_cmd(args.team, 'set', '--endpoint', 'https://recb-unreachable.example.invalid', '--bucket', args.bucket, '--region', args.region,
                             '--access-key-id', access, '--path-style', '--prefix', prefix, secret=secret)
        tb.must_not_leak(secret, down)
        if down.returncode == 0 or 'write step' not in (down.stdout or '') + (down.stderr or '') or (tb.configured(args.team) or {}).get('endpoint') != args.endpoint:
            raise RuntimeError('an unreachable bucket was not refused naming its step, or it replaced the saved bucket')
        receipt['phases'].append('unreachable_bucket_named')
        with winhost.workdir('paperboat-bucket-') as directory:
            recording, objects = tb.phase_archive(args, access, secret, prefix, directory)
            receipt['archived_recording'], receipt['archived_objects'] = recording, objects
            receipt['phases'] += ['recordings_archived', 'objects_are_ciphertext', 'replay_from_bucket']
            online = export_archive(recording, directory)
            shell(args.stop_cmd)
            stopped = True
            rec.wait_for('the target to be listed offline', lambda: machine_state(args.team, args.target) == 'offline', timeout=600, interval=10)
            offline = export_archive(recording, directory)
            if not offline or offline != online or b'"o"' not in offline:
                raise RuntimeError('the replay from the bucket while the machine is offline differs from the online one')
            receipt['phases'].append('cli_replay_machine_offline')
            if args.browser_cmd:
                shell(args.browser_cmd, {'REC_ID': recording, 'TEAM': args.team})
                receipt['phases'].append('browser_replay_machine_offline')
    finally:
        if stopped:
            shell(args.start_cmd)
            rec.wait_for('the target to be online again', lambda: machine_state(args.team, args.target) == 'online', timeout=600, interval=10)
        removed = tb.bucket_cmd(args.team, 'remove')
    if removed.returncode or tb.configured(args.team) is not None:
        raise RuntimeError('pb team bucket remove did not disconnect the bucket')
    receipt['phases'].append('bucket_removed')
    receipt['seconds'] = round(time.monotonic() - started, 1)
    print(json.dumps(receipt), flush=True)


if __name__ == '__main__':
    main()
