#!/usr/bin/env python3
"""Send only the two synthetic manifests requested by the receiver fixture."""
import argparse
import json
from pathlib import Path
import tempfile

from inbox_approval import PAYLOAD
from install_lifecycle import registration
from pairing_lifecycle import command_result, invoke


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--phase', required=True)
    args = parser.parse_args()
    phase = json.loads(args.phase)
    assert phase['state'] == 'inbox_sender_required' and len(phase['files']) == 2
    before = registration()
    assert before['machine_id'] == phase['source_machine_id']
    binary = Path.home() / '.local/bin/pb'
    auth = invoke(binary, 'auth', 'status', '--json')
    assert auth['signed_in'] and auth['account']['id'] == phase['sender_account']
    with tempfile.TemporaryDirectory(prefix='paperboat-inbox-sender-') as directory:
        for filename, approved in zip(phase['files'], [True, False]):
            assert filename.startswith('smoke-inbox-') and Path(filename).name == filename
            source = Path(directory) / filename
            source.write_bytes(PAYLOAD)
            arguments = ['--workspace', phase['team_id'], 'send', str(source),
                         '--to', phase['target'], '--json']
            if approved:
                result = invoke(binary, *arguments, timeout=180)
                assert len(result['transfers']) == 1
            else:
                result = command_result(binary, *arguments, timeout=180)
                assert result.returncode != 0
                receipt = json.loads(result.stdout)
                assert receipt.get('ok') is False
                assert 'declined' in receipt['error']['message']
    assert registration() == before
    print(json.dumps({'check': 'inbox-sender', 'status': 'PASS',
                      'approval_acknowledged': True, 'decline_acknowledged': True}))


if __name__ == '__main__':
    main()
