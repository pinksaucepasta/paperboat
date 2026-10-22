#!/usr/bin/env python3
"""C001: `pb approve <machine>` says why it did not approve, with a code and nothing changed.

Runs on an enrolled machine. An unknown id is machine_not_found; this machine's own (already approved)
enrollment is approval_not_pending. The success path needs a pending enrollment, which the owner daemon
approves on its own, so it stays covered by the unit tests.
"""
import argparse
import json
from pathlib import Path
import subprocess

from install_lifecycle import registration

PB = Path.home() / '.local/bin/pb'


def approve(machine):
    result = subprocess.run([str(PB), 'approve', machine, '--json'], capture_output=True, text=True,
                            timeout=60, stdin=subprocess.DEVNULL)
    try:
        error = json.loads(result.stdout).get('error', {})
    except ValueError:
        raise RuntimeError('pb approve %s did not return JSON (exit %d)' % (machine[:20], result.returncode)) from None
    return result.returncode, error


def expect(machine, code):
    status, error = approve(machine)
    if status == 0 or error.get('code') != code or error.get('state_changed') is not False or not error.get('message'):
        raise RuntimeError('pb approve was not refused as %s with nothing changed: exit %d code %r state_changed %r'
                           % (code, status, error.get('code'), error.get('state_changed')))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target', nargs='?')
    parser.parse_args()
    before = registration()
    expect('machine_00000000-0000-4000-8000-000000000000', 'machine_not_found')
    expect(before['machine_id'], 'approval_not_pending')
    if registration() != before:
        raise RuntimeError('refused approvals changed the enrolled identity')
    print(json.dumps({'check': 'approve-refusals', 'status': 'PASS', 'refusals': ['machine_not_found', 'approval_not_pending'],
                      'enrollment_preserved': True}))


if __name__ == '__main__':
    main()
