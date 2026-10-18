#!/usr/bin/env python3
"""F12-04 / F12-05: share a personal machine into a team, grant, revoke, unshare; owner activity shows it.

The enrolled device account is the machine owner and a MEMBER of TEAM; the controlled second profile
(teammate_profile.py) is the team OWNER. Usage: team_machines.py TEAM
"""
import argparse
import json
from pathlib import Path

from install_lifecycle import registration
from pairing_lifecycle import invoke
from teammate_profile import environment, profile_root

PB = Path.home() / '.local/bin/pb'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('team')
    args = parser.parse_args()
    root = profile_root()
    config, env = root / 'config.json', environment(root)

    def member(*arguments):
        value = json.loads(invoke(PB, *arguments, raw=True, input='', timeout=60))
        return value.get('data', value) if isinstance(value, dict) else value

    def owner(*arguments):
        value = json.loads(invoke(PB, '--config', str(config), *arguments, raw=True, input='', env=env, timeout=60))
        return value.get('data', value) if isinstance(value, dict) else value

    me = registration()
    machine_id = me['machine_id']
    owner_id = owner('auth', 'status', '--json')['account']['id']
    member_id = member('auth', 'status', '--json')['account']['id']
    team = args.team

    def generation():
        return str(member('team', 'get', team, '--json')['generation'])

    def seen_by_owner():
        machines = owner('--workspace', team, 'machine', 'list', '--json')['machines']
        return next((m for m in machines if m['id'] == machine_id), None)

    if not any(m['account_id'] == member_id and m['active'] for m in member('team', 'get', team, '--json')['members']):
        raise RuntimeError('device account must be an active member of the team')
    if seen_by_owner() is not None:
        raise RuntimeError('machine is already shared with the team; refusing to guess its prior grants')
    shared = False
    try:
        member('team', 'machine', 'share', team, machine_id, '--generation', generation(), '--json')
        shared = True
        if seen_by_owner() is not None:
            raise RuntimeError('sharing alone granted the owner machine access')
        member('team', 'machine', 'grant', team, machine_id, '--member', owner_id, '--capability',
               'terminal,exec', '--generation', generation(), '--json')
        view = seen_by_owner()
        if view is None or not {'terminal', 'exec'} <= set(view['permissions']):
            raise RuntimeError('owner does not see the granted capabilities')
        if 'files' in view['permissions']:
            raise RuntimeError('owner sees a capability that was not granted')
        member('team', 'machine', 'grant', team, machine_id, '--all-members', '--capability', 'files',
               '--generation', generation(), '--json')
        if 'files' not in (seen_by_owner() or {}).get('permissions', []):
            raise RuntimeError('all-member grant did not reach the owner')
        member('team', 'machine', 'grant', team, machine_id, '--all-members', '--capability', 'files',
               '--active=false', '--generation', generation(), '--json')
        member('team', 'machine', 'grant', team, machine_id, '--member', owner_id, '--capability',
               'terminal,exec', '--active=false', '--generation', generation(), '--json')
        if seen_by_owner() is not None:
            raise RuntimeError('revoked grants still show the machine to the owner')
        member('team', 'machine', 'unshare', team, machine_id, '--generation', generation(), '--json')
        shared = False
        if seen_by_owner() is not None:
            raise RuntimeError('unshared machine is still visible to the owner')
        activity = owner('team', 'activity', team, '--limit', '50', '--json')
        events = [e for e in activity['items'] if e.get('metadata', {}).get('machine_id') == machine_id]
        kinds = sorted({e['action'] for e in events})
        for needed in ('machine_share', 'machine_grant', 'machine_unshare'):
            if needed not in kinds:
                raise RuntimeError('team activity lacks %s for this machine; saw: %s' % (needed, ', '.join(kinds)[:200]))
    finally:
        if shared:
            member('team', 'machine', 'unshare', team, machine_id, '--generation', generation(), '--json')
    if registration() != me:
        raise RuntimeError('team machine commands changed the enrollment')
    print(json.dumps({'check': 'team-machines', 'status': 'PASS', 'share_hides_until_granted': True,
                      'grant_and_all_member_grant_effective': True, 'revoke_and_unshare_hide': True,
                      'activity_has_machine_events': True, 'activity_kinds': kinds[:8]}), flush=True)


if __name__ == '__main__':
    main()
