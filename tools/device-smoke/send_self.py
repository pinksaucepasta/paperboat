#!/usr/bin/env python3
"""Send a file to this same computer: it lands in the local Inbox, byte-identical, with a transfer identity."""
import argparse
import hashlib
import json
from pathlib import Path
import tempfile
import uuid

from install_lifecycle import registration
from pairing_lifecycle import invoke
import winhost


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    binary = winhost.pb_path()
    device = winhost.device()  # the Windows device: the source file and the Inbox are there
    before = registration()
    machines = invoke(binary, '--workspace', 'personal', 'machine', 'list', '--all', '--json')
    machines = machines.get('machines', machines.get('items', []))
    own = [m for m in machines if m['id'] == before['machine_id']]
    if len(own) != 1:
        raise RuntimeError('this computer is not in the account machine list exactly once')
    prefix = 'paperboat-smoke-self-' + uuid.uuid4().hex
    line = ('λ船 ' + prefix + '\n').encode()
    content = line * 5000
    inbox = winhost.RemotePath(device, before['inbox_path']) if device else Path(before['inbox_path']).expanduser()
    delivered = inbox / (prefix + '.txt')
    failure = None
    with winhost.workdir('paperboat-send-self-') as directory:
        source = directory / (prefix + '.txt')
        winhost.write_pattern(source, line, 5000)
        try:
            batch = invoke(binary, 'send', str(source), '--to', own[0]['alias'], '--json', timeout=120)
            if len(batch['transfers']) != 1:
                raise RuntimeError('send to this computer did not report exactly one transfer')
            arrived = (delivered.exists() and delivered.sha256()) if device else (delivered.is_file() and hashlib.sha256(delivered.read_bytes()).hexdigest())
            if arrived != hashlib.sha256(content).hexdigest():
                raise RuntimeError('the file is not in the local Inbox, byte-identical')
            transfer = batch['transfers'][0]
            if transfer.get('state') != 'delivered' or winhost.device_path(transfer.get('receipt_path', '')) != delivered:
                raise RuntimeError('the receipt does not name the delivered Inbox file')
            if not transfer.get('transfer_id'):
                raise RuntimeError('the delivery has no transfer identity, so it has no activity record to list or download')
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
                      'local_copy': True, 'recorded': True, 'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
