#!/usr/bin/env python3
"""Receive two real teammate sends: approve one, decline one, restore policy."""
import argparse
import hashlib
import json
from pathlib import Path
import tempfile
import time
import uuid

from install_lifecycle import registration
from pairing_lifecycle import invoke
from teammate_profile import environment, profile_root

PAYLOAD = b'Paperboat controlled teammate Inbox fixture\n'


def decide_manifests(call, source_id, destination_id, files, inbox, refused):
    for filename, action in zip(files, ['approve', 'decline']):
        deadline = time.monotonic() + 150
        while True:
            if refused.exists():
                raise RuntimeError('teammate sender failed; private sender receipt retained')
            requests = call('inbox', 'requests', '--json')['requests']
            matching = [r for r in requests if r['source_machine_id'] == source_id
                        and r['destination_machine_id'] == destination_id
                        and [f['basename'] for f in r['files']] == [filename]]
            if matching:
                assert len(matching) == 1
                request = matching[0]
                break
            if time.monotonic() >= deadline:
                raise RuntimeError('teammate send did not reach its exact pending Inbox request')
            time.sleep(.2)
        assert request['status'] == 'pending' and not (inbox / filename).exists()
        decision = call('inbox', action, request['request_id'],
                        '--generation', str(request['decision_generation']), '--json')
        assert decision['status'] == ('approved' if action == 'approve' else 'declined')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source-machine-id', required=True)
    args = parser.parse_args()
    binary = Path.home() / '.local/bin/pb'
    before = registration()
    root = profile_root()

    def call(*values, teammate=False):
        prefix = ['--config', str(root / 'config.json')] if teammate else []
        value = json.loads(invoke(binary, *prefix, *values, raw=True, input='',
                                  env=environment(root) if teammate else None, timeout=35))
        return value.get('data', value) if isinstance(value, dict) else value

    recipient = call('auth', 'status', '--json', teammate=True)['account']
    assert recipient['id'] != before['account_id']
    name = 'smoke-inbox-' + uuid.uuid4().hex
    team = name[:45]
    files = [name + '-approved.txt', name + '-declined.txt']
    inbox = Path(call('inbox', 'path', '--json')['path'])
    policy = call('inbox', 'policy', '--json')
    created = False
    failure = None
    try:
        current = call('team', 'create', team, '--json')
        created = True
        invitation = call('team', 'invite', team, recipient['email'],
                          '--generation', str(current['generation']), '--json')
        current = call('team', 'accept', invitation['invitation_id'], '--json', teammate=True)
        current = call('team', 'machine', 'share', team, before['machine_id'],
                       '--generation', str(current['generation']), '--json')
        call('team', 'machine', 'grant', team, before['machine_id'], '--member', recipient['id'],
             '--capability', 'files', '--generation', str(current['generation']), '--json')
        call('inbox', 'policy', 'manual', '--json')
        with tempfile.TemporaryDirectory(prefix='paperboat-inbox-approval-') as directory:
            gate, refused = Path(directory) / 'complete', Path(directory) / 'failed'
            print(json.dumps({'state': 'inbox_sender_required', 'team_id': team,
                              'target': before['machine_id'], 'files': files,
                              'source_machine_id': args.source_machine_id,
                              'sender_account': recipient['id'],
                              'gate': str(gate), 'failure_gate': str(refused)}), flush=True)
            decide_manifests(call, args.source_machine_id, before['machine_id'], files, inbox, refused)
            deadline = time.monotonic() + 150
            while not gate.exists():
                if refused.exists() or time.monotonic() >= deadline:
                    raise RuntimeError('teammate sends did not finish their bounded acknowledgement')
                time.sleep(.1)
            assert hashlib.sha256((inbox / files[0]).read_bytes()).digest() == hashlib.sha256(PAYLOAD).digest()
            assert not (inbox / files[1]).exists()
    except BaseException as error:
        failure = error
        raise
    finally:
        try:
            options = ['--receipt-email'] if policy['receipt_email'] else []
            call('inbox', 'policy', policy['acceptance'], *options, '--json')
            if created:
                current = call('team', 'get', team, '--json')
                assert current['owner_account'] == before['account_id']
                call('team', 'delete', team, '--generation', str(current['generation']),
                     '--confirm', team, '--json')
            for filename in files:
                (inbox / filename).unlink(missing_ok=True)
        except BaseException:
            if failure is None:
                raise
            print(json.dumps({'check': 'inbox-approval', 'cleanup': 'failed'}), flush=True)
    assert registration() == before
    print(json.dumps({'check': 'inbox-approval', 'status': 'PASS', 'approved_delivery': True,
                      'declined_no_delivery': True, 'policy_restored': True,
                      'enrollment_preserved': True}), flush=True)


if __name__ == '__main__':
    main()
