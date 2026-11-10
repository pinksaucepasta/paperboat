#!/usr/bin/env python3
"""F1-10 / C012 / C020: `pb reset` then `pb pair` on the same machine enrolls it again and replaces the old record.

Usage: reset_lifecycle.py reset|pair (sudo password on stdin). Needs a signed-in owner CLI on an enrolled machine.
Two phases because a reset signs this computer out, so only ANOTHER signed-in device of the account can list machines
in between: `reset` wipes the setup and interrupts the first pair (failure injection) and prints the old record's id,
name and address; the caller checks that record is still on the server; `pair` then finishes the pairing and checks
the replacement. State between the phases is a 0700 directory.
"""
import json
import os
from pathlib import Path
import shutil
import sys
import tempfile
import time

import pairing_lifecycle as p
import winhost

PB = winhost.pb_path()
STATE = Path(tempfile.gettempdir()) / ('pb-reset-state-%d' % os.getuid())
# The token file is read by pb, so on the Windows device it lives there; the phase state stays on this controller.
TOKENS = winhost.device().home() / 'pb-reset-state' if winhost.device() else STATE


def machines():
    return p.invoke(PB, '--workspace', 'personal', 'machine', 'list', '--all', '--json')['machines']


def reset_phase(password):
    root = p.runtime_root()
    before = json.loads((root / 'machine-registration.json').read_text())
    original = next(m for m in machines() if m['id'] == before['machine_id'])
    alias = original['alias']
    if not original.get('assigned_ip'):
        raise RuntimeError('the old record has no assigned_ip: the check cannot prove the local address is kept')
    inbox = winhost.device_path(before['inbox_path'])
    sentinel = inbox / ('.paperboat-f1-reset-' + os.urandom(6).hex())
    inbox.mkdir(parents=True, exist_ok=True)
    sentinel.write_bytes(os.urandom(32))
    # Listing needs the vault unlocked; a locked vault means no environment names are known, so the
    # pending-environment explanation is not required of pair.
    listed = p.command_result(PB, 'env', 'list', '--json')
    try:
        had_env = listed.returncode == 0 and bool(json.loads(listed.stdout).get('names'))
    except ValueError:
        had_env = False
    shutil.rmtree(STATE, ignore_errors=True)
    STATE.mkdir(mode=0o700)
    if winhost.device():
        TOKENS.unlink()
        TOKENS.mkdir()
    token = p.enrollment_file(PB, alias, TOKENS)
    # Confirmation is exact, and a wrong hostname must be refused without changing anything.
    wrong = p.command_result(PB, 'reset', '--enrollment-token-file', str(token), '--confirmation',
                             'RESET PAPERBOAT', '--hostname', 'not-this-host', '--json',
                             terminal=True, password=password)
    if wrong.returncode == 0:
        raise RuntimeError('reset accepted the wrong hostname')
    if json.loads((root / 'machine-registration.json').read_text())['machine_id'] != before['machine_id']:
        raise RuntimeError('refused reset still changed the enrollment')
    reset = p.invoke(PB, 'reset', '--enrollment-token-file', str(token), '--confirmation',
                     'RESET PAPERBOAT', '--hostname', p.hostname(PB), '--json',
                     raw=True, terminal=True, password=password)
    if '"reset":true' not in reset.replace(' ', ''):
        raise RuntimeError('reset did not report reset')
    # Failure injection (make-before-break, 33a59051f): a pair interrupted after the reset must leave the old
    # machine record, its name and its local address on the server; only a pair that works replaces it.
    try:
        p.interrupt_pair(PB, ['pair', '--server', before['server_url'], '--enrollment-token-file', str(token),
                              '--name', alias], root, 'sigint', None)
    except p.FaultAbsorbed:
        raise RuntimeError('the interrupted pair completed; the failure injection did not fail it')
    (STATE / 'state.json').write_text(json.dumps({'before': before, 'original': original, 'sentinel': str(sentinel),
                                                  'had_env': had_env, 'started': time.time()}))
    print(json.dumps({'phase': 'reset', 'old_machine_id': before['machine_id'], 'alias': alias,
                      'assigned_ip': original.get('assigned_ip')}), flush=True)


def pair_phase(password):
    root = p.runtime_root()
    state = json.loads((STATE / 'state.json').read_text())
    before, original, had_env = state['before'], state['original'], state['had_env']
    alias, sentinel = original['alias'], winhost.device_path(state['sentinel'])
    try:
        token = TOKENS / 'enrollment-token'
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
        if new.get('assigned_ip') != original['assigned_ip']:
            failures.append('local address changed from %s to %s' % (original['assigned_ip'], new.get('assigned_ip')))
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline and p.command_result(PB, 'exec', alias, '--', *winhost.true_argv()).returncode:
            time.sleep(4)
        if not p.doctor_healthy(p.invoke(PB, 'doctor', '--json', timeout=60)):
            raise RuntimeError('machine is not healthy after reset + pair')
        if not sentinel.exists():
            raise RuntimeError('reset removed an Inbox file')
    finally:
        sentinel.unlink(missing_ok=True)
        shutil.rmtree(STATE, ignore_errors=True)
        if winhost.device():
            TOKENS.unlink()
    if failures:
        print(json.dumps({'check': 'reset-lifecycle', 'status': 'FAIL', 'failures': failures, 'old_machine_id': before['machine_id'], 'pair_env_lines': [l.strip() for l in paired.splitlines() if 'nvironment' in l or 'ENV' in l][:3], 'new_machine_id': after['machine_id']}), flush=True)
        raise SystemExit(1)
    print(json.dumps({'check': 'reset-lifecycle', 'status': 'PASS', 'seconds': round(time.time() - state['started'], 1),
                      'wrong_hostname_refused': True, 'new_record_same_alias': True, 'old_record_removed': True, 'local_address_kept': True,
                      'inbox_preserved': True}), flush=True)


def main():
    phases = {'reset': reset_phase, 'pair': pair_phase}
    if len(sys.argv) != 2 or sys.argv[1] not in phases:
        raise SystemExit('usage: reset_lifecycle.py reset|pair')
    phases[sys.argv[1]](None if winhost.device() else sys.stdin.readline().rstrip('\n'))


if __name__ == '__main__':
    main()
