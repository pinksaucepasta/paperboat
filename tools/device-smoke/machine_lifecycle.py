#!/usr/bin/env python3
"""Exercise normal machine rename and policy commands, restoring original settings."""
import argparse
import json
from pathlib import Path
import sys
import time
import uuid

from install_lifecycle import registration
from pairing_lifecycle import check_machine_revoked, confirmed_command, denied_exec, invoke

COMMAND_TIMEOUT = 45
POLICY_APPLY_TIMEOUT = 30


def restore_all(primary, *actions):
    errors = []
    for action in actions:
        try:
            action()
        except Exception as error:
            errors.append(error)
    if errors:
        reason = '; '.join(str(error) for error in errors)
        if primary is not None:
            reason = str(primary) + '; restoration also failed: ' + reason
        raise RuntimeError(reason) from (primary or errors[0])


def machine(pb, machine_id, timeout=COMMAND_TIMEOUT):
    catalog = invoke(pb, '--workspace', 'personal', 'machine', 'list', '--json', timeout=timeout)
    return next(item for item in catalog['machines'] if item['id'] == machine_id)


def rename(pb, original):
    machine_id, alias = original['id'], original['alias']
    temporary = 'smoke-' + uuid.uuid4().hex[:12]
    try:
        result = invoke(pb, 'machine', 'rename', machine_id, temporary, '--json')
        if result['outcome'] != 'renamed' or result['machine']['alias'] != temporary:
            raise RuntimeError('rename receipt did not identify the new alias')
        if machine(pb, machine_id)['alias'] != temporary:
            raise RuntimeError('machine list did not publish the renamed alias')
        invoke(pb, 'exec', temporary, '--', 'true', raw=True, timeout=COMMAND_TIMEOUT)
        for attempt in range(12):  # status reads the daemon snapshot, refreshed from the server's inventory within ~15 s
            try:
                status = invoke(pb, 'status', temporary, '--json')
                break
            except RuntimeError:
                if attempt == 11:
                    raise
                time.sleep(5)
        if len(status['machines']) != 1 or status['machines'][0]['id'] != machine_id:
            raise RuntimeError('status did not resolve the renamed machine')
    finally:
        restore_all(sys.exc_info()[1],
                    lambda: invoke(pb, 'machine', 'rename', machine_id, alias, '--json'))


def wait_policy(pb, machine_id, kind, version, expected):
    deadline = time.monotonic() + POLICY_APPLY_TIMEOUT
    while time.monotonic() < deadline:
        policy = machine(pb, machine_id, max(1, deadline - time.monotonic()))[kind]
        observed = (('applied_version', 'applied') if kind == 'machine_capabilities'
                    else ('observed_version', 'observed_mode'))
        if policy['status'] == 'applied' and policy[observed[0]] == version and policy[observed[1]] == expected:
            return
        if policy['status'] == 'error':
            raise RuntimeError(kind + ' rejected by runtime: ' + str(policy.get('error_code')))
        time.sleep(1)
    raise RuntimeError(kind + ' was not applied within ' + str(POLICY_APPLY_TIMEOUT) + ' seconds')


def availability(pb, machine_id, mode):
    args = ['machine', 'availability', machine_id, '--mode', mode.replace('_', '-')]
    if mode == 'keep_awake':
        result = confirmed_command(pb, *args, timeout=COMMAND_TIMEOUT)
    else:
        result = invoke(pb, *args, '--json', timeout=COMMAND_TIMEOUT)
    policy = result['availability']
    wait_policy(pb, machine_id, 'availability', policy['desired_version'], mode)


def capabilities(pb, machine_id, selection):
    args = ['machine', 'capabilities', machine_id, '--json']
    args += ['--' + key.replace('_', '-') + '=' + str(value).lower()
             for key, value in selection.items()]
    policy = invoke(pb, *args, timeout=COMMAND_TIMEOUT)['machine_capabilities']
    wait_policy(pb, machine_id, 'machine_capabilities', policy['desired_version'], selection)


def settings(pb, original):
    machine_id = original['id']
    availability_only(pb, original)
    try:
        invoke(pb, 'exec', original['alias'], '--', 'true', raw=True, timeout=COMMAND_TIMEOUT)
        capabilities(pb, machine_id, {key: False for key in original['machine_capabilities']['desired']})
        denied_exec(pb, original['alias'], {'transport_unavailable'})
    finally:
        restore_all(sys.exc_info()[1],
                    lambda: capabilities(pb, machine_id, original['machine_capabilities']['desired']))
    invoke(pb, 'exec', original['alias'], '--', 'true', raw=True, timeout=COMMAND_TIMEOUT)


def availability_only(pb, original):
    try:
        availability(pb, original['id'], 'keep_awake')
        availability(pb, original['id'], 'allow_sleep')
    finally:
        restore_all(sys.exc_info()[1],
                    lambda: availability(pb, original['id'], original['availability']['desired_mode']))


def individual_capabilities(pb, original):
    baseline = original['machine_capabilities']['desired']
    if not all(baseline.values()):
        raise RuntimeError('individual capability check requires the enabled baseline; nothing changed')
    invoke(pb, 'exec', original['alias'], '--json', '--', 'true',
           raw=True, timeout=COMMAND_TIMEOUT)
    for name in baseline:
        selection = dict(baseline, **{name: False})
        try:
            capabilities(pb, original['id'], selection)
            if name == 'terminal':
                denied_exec(pb, original['alias'], {'transport_unavailable'})
        finally:
            restore_all(sys.exc_info()[1], lambda: capabilities(pb, original['id'], baseline))
        print(json.dumps({'check': 'individual-capability', 'name': name,
                          'status': 'PASS', 'original_selection_restored': True}), flush=True)
    invoke(pb, 'exec', original['alias'], '--json', '--', 'true',
           raw=True, timeout=COMMAND_TIMEOUT)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('check', choices=['rename', 'settings', 'availability', 'capabilities', 'revoke'])
    parser.add_argument('--machine-id')
    parser.add_argument('--expected-account')
    args = parser.parse_args()
    if args.check == 'revoke' and (not args.machine_id or not args.expected_account):
        parser.error('revoke requires --machine-id and --expected-account from the waiting target')
    pb, enrolled = Path.home() / '.local/bin/pb', registration()
    original = machine(pb, enrolled['machine_id'])
    if args.check == 'revoke':
        target = machine(pb, args.machine_id)
        if (target['id'] == enrolled['machine_id'] or not args.expected_account or
                target['owner_account'] != args.expected_account or
                enrolled['account_id'] != args.expected_account):
            raise RuntimeError('revoke controller requires another machine of the exact same account')
        invoke(pb, 'exec', target['id'], '--', 'true', raw=True, timeout=COMMAND_TIMEOUT)
        check_machine_revoked(pb, target['id'], perform=True)
    else:
        {'rename': rename, 'settings': settings,
         'availability': availability_only, 'capabilities': individual_capabilities}[args.check](pb, original)
    if registration() != enrolled:
        raise RuntimeError('machine command changed the protected enrollment')
    if machine(pb, enrolled['machine_id'])['alias'] != original['alias']:
        raise RuntimeError('original machine alias was not restored')
    print(json.dumps({'check': 'machine-' + args.check, 'status': 'PASS',
                      'enrollment_preserved': True, 'original_settings_restored': True}))


if __name__ == '__main__':
    main()
