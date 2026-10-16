#!/usr/bin/env python3
"""Verify owner-side terminal grants using only an owned, empty temporary team."""
import argparse
import json
from pathlib import Path
import sys
import uuid

from install_lifecycle import registration
from machine_lifecycle import restore_all
from pairing_lifecycle import command_diagnostic, command_result, invoke
from session_cleanup import cleanup
from session_lifecycle import selected
from terminal_lifecycle import Terminal


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    pb = Path.home() / '.local/bin/pb'
    name = 'smoke-session-' + uuid.uuid4().hex[:12]
    team = 'smoke-sharing-' + uuid.uuid4().hex[:12]
    terminal = None
    identifier = None
    created = False
    unshared = False
    prefix = ('--workspace', 'personal')

    def end_sharing():
        if identifier is not None and not unshared:
            invoke(pb, *prefix, 'session', 'unshare', identifier, '--json')

    def delete_team():
        if created:
            current = invoke(pb, *prefix, 'team', 'get', team, '--json')
            invoke(pb, *prefix, 'team', 'delete', team, '--generation',
                   str(current['generation']), '--confirm', team, '--json')

    try:
        invoke(pb, *prefix, 'team', 'create', team, '--json')
        created = True
        terminal = Terminal([pb, *prefix, 'connect', args.target, '--name', name, '--status-bar', 'off'])
        terminal.expect(b'\x1b[?2004h')
        terminal.expect(terminal.marker('READY', 'stty -echo; '))
        _, session = selected(pb, args.target, name)
        identifier = session['id']
        for role in ['viewer', 'interactive']:
            grant = invoke(pb, *prefix, 'session', 'share', identifier, '--team', team,
                           '--all', '--role', role, '--json')
            if grant['sharing']['team_id'] != team or not any(
                    g['active'] and g['audience'] == 'all_members' and g['role'] == role
                    for g in grant['sharing']['grants']):
                raise RuntimeError('sharing did not apply the exact requested grant')
            state = invoke(pb, *prefix, 'session', 'participants', identifier, '--json')
            if state['id'] != identifier or not state['can_manage'] or not state['participants_available']:
                raise RuntimeError('participants did not identify the live owned terminal')
            if not any(p['account_id'] == registration()['account_id'] for p in state['participants']):
                raise RuntimeError('live participant owner identity was missing')
        result = command_result(pb, *prefix, 'session', 'shared', '--json')
        if result.returncode:
            raise RuntimeError('shared catalog failed; private diagnostics: ' + str(
                command_diagnostic(result.args, result.stdout, result.stderr)))
        catalog = json.loads(result.stdout)
        catalog = catalog.get('data', catalog) if isinstance(catalog, dict) else catalog
        if not any(s['id'] == identifier for s in catalog):
            raise RuntimeError('shared catalog omitted the exact owned session')
        state = invoke(pb, *prefix, 'session', 'unshare', identifier, '--json')
        if any(g['active'] for g in state['sharing']['grants']):
            raise RuntimeError('ending sharing retained an active grant')
        unshared = True
        terminal.expect(terminal.marker('OWNER_SURVIVES_UNSHARE'))
    finally:
        restore_all(sys.exc_info()[1], end_sharing,
                    *([terminal.close] if terminal is not None else []),
                    lambda: cleanup(str(pb), args.target, name), delete_team)
    print(json.dumps({'check': 'session-sharing-owner', 'target': args.target, 'status': 'PASS',
                      'viewer_grant': True, 'interactive_grant': True, 'live_participants': True,
                      'shared_catalog': True, 'owner_survives_unshare': True,
                      'exact_fixture_cleanup': True}), flush=True)


if __name__ == '__main__':
    main()
