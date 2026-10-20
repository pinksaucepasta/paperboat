#!/usr/bin/env python3
"""F1-10 / C012 / C020: `pb reset` then `pb pair` on the same machine enrolls it again and replaces the old record.

Usage: reset_lifecycle.py (sudo password on stdin). Needs a signed-in owner CLI on an enrolled machine.
"""
import json
import os
from pathlib import Path
import sys
import tempfile
import time

import pairing_lifecycle as p

PB = Path.home() / '.local/bin/pb'


def machines():
    return p.invoke(PB, '--workspace', 'personal', 'machine', 'list', '--json')['machines']


def main():
    password = sys.stdin.readline().rstrip('\n')
    root = p.runtime_root()
    before = json.loads((root / 'machine-registration.json').read_text())
    original = next(m for m in machines() if m['id'] == before['machine_id'])
    alias = original['alias']
    inbox = Path(before['inbox_path'])
    sentinel = inbox / ('.paperboat-f1-reset-' + os.urandom(6).hex())
    inbox.mkdir(parents=True, exist_ok=True)
    sentinel.write_bytes(os.urandom(32))
    started = time.monotonic()
    # Listing needs the vault unlocked; a locked vault means no environment names are known, so the
    # pending-environment explanation is not required of pair.
    listed = p.command_result(PB, 'env', 'list', '--json')
    try:
        had_env = listed.returncode == 0 and bool(json.loads(listed.stdout).get('names'))
    except ValueError:
        had_env = False
    try:
        with tempfile.TemporaryDirectory(prefix='pb-reset-') as directory:
            token = p.enrollment_file(PB, alias, Path(directory))
            # Confirmation is exact, and a wrong hostname must be refused without changing anything.
            wrong = p.command_result(PB, 'reset', '--enrollment-token-file', str(token), '--confirmation',
                                     'RESET PAPERBOAT', '--hostname', 'not-this-host', '--json',
                                     terminal=True, password=password)
            if wrong.returncode == 0:
                raise RuntimeError('reset accepted the wrong hostname')
            if json.loads((root / 'machine-registration.json').read_text())['machine_id'] != before['machine_id']:
                raise RuntimeError('refused reset still changed the enrollment')
            reset = p.invoke(PB, 'reset', '--enrollment-token-file', str(token), '--confirmation',
                             'RESET PAPERBOAT', '--hostname', os.uname().nodename, '--json',
                             raw=True, terminal=True, password=password)
            if '"reset":true' not in reset.replace(' ', ''):
                raise RuntimeError('reset did not report reset')
            paired = p.pair(PB, ['pair', '--server', before['server_url'], '--enrollment-token-file', str(token),
                        '--name', alias], root, 'none', before['server_url'], password)
        failures = []
        explained = 'managed environment variables are pending' in paired or 'ENV' in paired and 'pending' in paired.lower()
        if had_env and not explained:
            failures.append('pair output does not explain pending managed environment variables')
        after = json.loads((root / 'machine-registration.json').read_text())
        current = machines()
        new = next((m for m in current if m['id'] == after['machine_id']), None)
        if after['machine_id'] == before['machine_id'] or new is None:
            raise RuntimeError('pair after reset did not create a new machine record')
        if after['account_id'] != before['account_id']:
            raise RuntimeError('new record is on another account')
        if new['alias'] != alias:
            failures.append('new record alias is %s, not %s' % (new['alias'], alias))
        # Decision: adding the same computer again REMOVES its old record (not disconnected, not renamed).
        old = next((m for m in current if m['id'] == before['machine_id']), None)
        if old is not None:
            failures.append('old machine record %s still exists (%s) after reset + pair' % (before['machine_id'], old['state']))
        # ... and the new record keeps the computer's local address, so pasted connection strings still work.
        if original.get('assigned_ip') and new.get('assigned_ip') != original['assigned_ip']:
            failures.append('local address changed from %s to %s' % (original['assigned_ip'], new.get('assigned_ip')))
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline and p.command_result(PB, 'exec', alias, '--', 'true').returncode:
            time.sleep(4)
        if p.invoke(PB, 'doctor', '--json', timeout=60)['overall'] != 'healthy':
            raise RuntimeError('machine is not healthy after reset + pair')
        if not sentinel.exists():
            raise RuntimeError('reset removed an Inbox file')
    finally:
        sentinel.unlink(missing_ok=True)
    if failures:
        print(json.dumps({'check': 'reset-lifecycle', 'status': 'FAIL', 'failures': failures, 'old_machine_id': before['machine_id'], 'pair_env_lines': [l.strip() for l in paired.splitlines() if 'nvironment' in l or 'ENV' in l][:3], 'new_machine_id': after['machine_id']}), flush=True)
        raise SystemExit(1)
    print(json.dumps({'check': 'reset-lifecycle', 'status': 'PASS', 'seconds': round(time.monotonic() - started, 1),
                      'wrong_hostname_refused': True, 'new_record_same_alias': True, 'old_record_removed': True, 'local_address_kept': True,
                      'inbox_preserved': True}), flush=True)


if __name__ == '__main__':
    main()
