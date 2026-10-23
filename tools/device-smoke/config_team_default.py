#!/usr/bin/env python3
"""F11-04 team defaults on one paired Linux machine: set, adopt, unadopt.

Runs ON the paired machine: `python3 config_team_default.py TEAM GITHUB_REPOSITORY`. The repository must
already be connected to this account as a GitHub config repository; the account must administer TEAM.
Adopting writes pending pull-only assignments to every unassigned machine of the account; they are never
applied without approval (this script approves nothing) and unadopt revokes them, so it ends unadopted.

Checks (one PASS/FAIL line each, JSON summary at the end):
  F11-04 team-default-set   the team default becomes the repository, version advances
  F11-04 team-default-adopt this account adopts that version
  F11-04 team-default-unadopt   adoption ends; a second unadopt is refused (nothing adopted)
"""
import json
import subprocess
import sys

results = {}


def pb(*args):
    out = subprocess.run(['pb', 'config', *args, '--json'], capture_output=True, text=True, timeout=180)
    try:
        return json.loads(out.stdout)
    except ValueError:
        raise AssertionError('not JSON: ' + (out.stdout + out.stderr)[-300:])


def check(name, action):
    try:
        results[name] = {'ok': True, 'detail': action()}
        print('PASS', name, '-', results[name]['detail'])
    except Exception as error:  # noqa: BLE001
        results[name] = {'ok': False, 'detail': '%s: %s' % (type(error).__name__, error)}
        print('FAIL', name, '-', results[name]['detail'])


def main():
    team, repository = sys.argv[1], sys.argv[2]
    state = {}

    def set_default():
        out = pb('team-default-set', team, repository)
        assert out.get('ok'), out.get('error')
        data = out['data']
        assert data['team_id'] == team and data['provider'] == 'github' and data['display_name'] == repository, data
        state['version'] = data['version']
        return 'version %d' % data['version']
    check('F11-04 team-default-set', set_default)

    def adopt():
        out = pb('team-default-adopt', team)
        assert out.get('ok'), out.get('error')
        data = out['data']
        assert data['adopted_version'] == state['version'] and data['repository_id'], data
        return 'adopted version %d' % data['adopted_version']
    check('F11-04 team-default-adopt', adopt)

    def unadopt():
        out = pb('team-default-unadopt', team)
        assert out.get('ok') and out['data'] == {'adopted': False}, out
        again = pb('team-default-unadopt', team)
        assert not again.get('ok'), 'a second unadopt succeeded'
        return 'unadopted; second unadopt refused'
    check('F11-04 team-default-unadopt', unadopt)

    print(json.dumps(results))
    sys.exit(0 if all(r['ok'] for r in results.values()) else 1)


if __name__ == '__main__':
    main()
