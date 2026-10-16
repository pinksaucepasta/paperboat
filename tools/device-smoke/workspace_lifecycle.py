#!/usr/bin/env python3
"""Verify selected workspace isolation using an empty temporary team, then remove it."""
import json
from pathlib import Path
import uuid

from install_lifecycle import registration
from pairing_lifecycle import invoke


def main():
    pb = Path.home() / '.local/bin/pb'
    enrolled = registration()
    team = 'smoke-f2-' + uuid.uuid4().hex[:12]
    personal = invoke(pb, '--workspace', 'personal', 'machine', 'list', '--json')
    expected = {machine['id'] for machine in personal['machines']}
    created = False
    try:
        invoke(pb, 'team', 'create', team, '--json')
        created = True
        invoke(pb, 'switch', team, raw=True)
        for command in ('machine', 'environments'):
            args = ('machine', 'list', '--json') if command == 'machine' else ('environments', '--json')
            if invoke(pb, *args)['machines']:
                raise RuntimeError(command + ' leaked personal machines into the empty team')
        if invoke(pb, 'status', '--json')['machines']:
            raise RuntimeError('local status leaked personal machines into the empty team')
        invoke(pb, 'auth', 'switch', 'personal', raw=True)
        actual = invoke(pb, 'machine', 'list', '--json')['machines']
        if {machine['id'] for machine in actual} != expected:
            raise RuntimeError('auth switch did not restore personal inventory')
        if registration() != enrolled:
            raise RuntimeError('workspace switching changed the machine enrollment')
        print(json.dumps({'check': 'workspace-switch', 'status': 'PASS',
                          'commands': ['switch', 'auth switch', 'machine list', 'environments', 'status'],
                          'personal_count': len(expected), 'empty_team_isolated': True,
                          'enrollment_preserved': True}))
    finally:
        invoke(pb, 'switch', 'personal', raw=True)
        if created:
            current = invoke(pb, '--workspace', 'personal', 'team', 'get', team, '--json')
            invoke(pb, '--workspace', 'personal', 'team', 'delete', team,
                   '--generation', str(current['generation']), '--confirm', team, '--json')


if __name__ == '__main__':
    main()
