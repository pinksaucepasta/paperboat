#!/usr/bin/env python3
"""Verify normal signed-in inventory against this device's enrolled identity."""
import json
from pathlib import Path

from install_lifecycle import registration
from pairing_lifecycle import invoke


def main():
    pb = Path.home() / '.local/bin/pb'
    enrolled = registration()
    auth = invoke(pb, 'auth', 'status', '--json')
    if not auth['signed_in'] or auth['account']['id'] != enrolled['account_id']:
        raise RuntimeError('active account differs from this machine enrollment')
    catalog = invoke(pb, 'machine', 'list', '--json')['machines']
    environments = invoke(pb, 'environments', '--json')['machines']
    identities = {machine['id'] for machine in catalog}
    if identities != {machine['id'] for machine in environments}:
        raise RuntimeError('machine list and environments disagree')
    for machines in (catalog, environments):
        own = next(machine for machine in machines
                   if machine['id'] == enrolled['machine_id'])
        if own['online'] is not True or own['state'] != 'online':
            raise RuntimeError('enrolled machine is not online in the inventory')
        for field in ('public_identity_key', 'installation_generation'):
            if own[field] != enrolled[field]:
                raise RuntimeError('inventory differs from enrolled ' + field)
        if own['can_manage'] is not True:
            raise RuntimeError('original account cannot manage its own machine')
    status = invoke(pb, 'status', '--json')
    if status['daemon_state'] != 'ready':
        raise RuntimeError('local daemon is not ready')
    if {machine['id'] for machine in status['machines']} != identities:
        raise RuntimeError('local status and server inventory disagree')
    if not any(machine['id'] == enrolled['machine_id'] and
               machine['environment_id'] == enrolled['environment_id']
               for machine in status['machines']):
        raise RuntimeError('local status lost the enrolled machine binding')
    if registration() != enrolled:
        raise RuntimeError('read-only inventory changed the machine enrollment')
    print(json.dumps({'check': 'account-inventory', 'status': 'PASS',
                      'commands': ['auth status', 'machine list', 'environments', 'status'],
                      'catalog_count': len(catalog), 'same_enrolled_account': True,
                      'same_machine_identity': True}))


if __name__ == '__main__':
    main()
