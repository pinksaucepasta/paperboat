#!/usr/bin/env python3
"""Send a file to this same computer: it lands in the local Inbox, byte-identical, and shows in the send history."""
import argparse
import hashlib
import json
from pathlib import Path
import tempfile
import uuid

from install_lifecycle import registration
from pairing_lifecycle import invoke


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    binary = Path.home() / '.local/bin/pb'
    before = registration()
    machines = invoke(binary, '--workspace', 'personal', 'machine', 'list', '--json')
    machines = machines.get('machines', machines.get('items', []))
    own = [m for m in machines if m['id'] == before['machine_id']]
    if len(own) != 1:
        raise RuntimeError('this computer is not in the account machine list exactly once')
    prefix = 'paperboat-smoke-self-' + uuid.uuid4().hex
    content = ('λ船 ' + prefix + '\n').encode() * 5000
    inbox = Path(before['inbox_path']).expanduser()
    delivered = inbox / (prefix + '.txt')
    failure = None
    with tempfile.TemporaryDirectory(prefix='paperboat-send-self-') as directory:
        source = Path(directory) / (prefix + '.txt')
        source.write_bytes(content)
        try:
            batch = invoke(binary, 'send', str(source), '--to', own[0]['alias'], '--json', timeout=120)
            if len(batch['transfers']) != 1:
                raise RuntimeError('send to this computer did not report exactly one transfer')
            if not delivered.is_file() or hashlib.sha256(delivered.read_bytes()).hexdigest() != hashlib.sha256(content).hexdigest():
                raise RuntimeError('the file is not in the local Inbox, byte-identical')
            listed = invoke(binary, 'send', 'list', '--q', source.name, '--json', timeout=60)
            items = listed.get('transfers', listed.get('items', []))
            if not items:
                raise RuntimeError('the delivery is missing from the send history')
        except Exception as error:
            failure = error
            raise
        finally:
            try:
                delivered.unlink(missing_ok=True)
            except OSError as error:
                if failure is None:
                    raise
                failure.add_note('Exact Inbox cleanup failed: ' + str(error))
    if registration() != before:
        raise RuntimeError('send to this computer changed the enrolled identity')
    print(json.dumps({'check': 'send-self', 'target': args.target, 'status': 'PASS',
                      'local_copy': True, 'in_history': True, 'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
