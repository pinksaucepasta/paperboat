#!/usr/bin/env python3
"""F6-05: sending too much, too many files, or to an offline computer is refused with a clear message and nothing is sent."""
import argparse
import json
from pathlib import Path
import subprocess
import tempfile
import uuid

from install_lifecycle import registration

PB = Path.home() / '.local/bin/pb'


def send(*paths, target):
    result = subprocess.run([str(PB), 'send', *map(str, paths), '--to', target, '--json'],
                            capture_output=True, text=True, timeout=180)
    try:
        value = json.loads(result.stdout)
    except ValueError:
        raise RuntimeError('send did not return JSON') from None
    return result.returncode, value.get('error', {}), result.stderr


def offline_machine():
    result = subprocess.run([str(PB), '--workspace', 'personal', 'machine', 'list', '--json'],
                            capture_output=True, text=True, timeout=60)
    machines = json.loads(result.stdout).get('machines', [])
    return next((m['alias'] for m in machines if not m['online'] and m['state'] in ('offline', 'disconnected')
                 and not m['alias'].startswith(('lanea-', 'smoke-'))), None)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target', help='a different, receiving computer')
    args = parser.parse_args()
    before = registration()
    prefix = 'paperboat-smoke-limits-' + uuid.uuid4().hex
    with tempfile.TemporaryDirectory(prefix=prefix) as directory:
        big = Path(directory) / 'too-big.bin'
        with big.open('wb') as handle:
            handle.truncate(51 << 20)  # a sparse 51 MiB file: one byte over the 50 MiB limit
        code, error, _ = send(big, target=args.target)
        if code == 0 or error.get('code') != 'file_transfer_limit_exceeded' or error.get('state_changed') is not False:
            raise RuntimeError('over-limit file was not refused as file_transfer_limit_exceeded with nothing sent: %r' % (error.get('code'),))
        if '50 MiB' not in error.get('message', '') or 'Nothing was sent' not in error.get('message', ''):
            raise RuntimeError('over-limit message does not state the limit and that nothing was sent')
        many = []
        for index in range(12):
            path = Path(directory) / ('f%02d' % index)
            path.write_text('x')
            many.append(path)
        code, error, _ = send(*many, target=args.target)
        if code == 0 or error.get('code') != 'file_transfer_limit_exceeded' or error.get('state_changed') is not False:
            raise RuntimeError('too many files were not refused as file_transfer_limit_exceeded: %r' % (error.get('code'),))
        if '12 files' not in error.get('message', '') or '10' not in error.get('message', ''):
            raise RuntimeError('too-many-files message does not state the count and the limit')
        offline = offline_machine()
        if offline is None:
            raise RuntimeError('the account has no offline computer to test the offline refusal with')
        small = Path(directory) / 'small.txt'
        small.write_text('hello')
        code, error, _ = send(small, target=offline)
        if code == 0 or error.get('code') != 'machine_offline' or error.get('state_changed') is not False:
            raise RuntimeError('an offline target was not refused as machine_offline with nothing sent: %r' % (error.get('code'),))
    if registration() != before:
        raise RuntimeError('refused sends changed the enrolled identity')
    print(json.dumps({'check': 'transfer-errors', 'target': args.target, 'status': 'PASS', 'over_limit': True,
                      'too_many_files': True, 'offline_target': True, 'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
