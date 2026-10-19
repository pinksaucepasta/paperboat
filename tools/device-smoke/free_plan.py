#!/usr/bin/env python3
"""F13-01 on a Free account: the paired machine works with no subscription or trial, on the allowance model.

Reads the CLI's own access credential in memory (never printed). Usage: free_plan.py TARGET
"""
import argparse
import glob
import json
import os
from pathlib import Path
import subprocess
import time
import urllib.request

HOME = Path.home()
PB = HOME / '.local/bin/pb'


def api(path, token, server):
    request = urllib.request.Request(server + path, headers={'Authorization': 'Bearer ' + token,
                                                             'X-Paperboat-Workspace': 'personal'})
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.loads(response.read())['data']


def run(*args):
    result = subprocess.run([str(PB), *args], capture_output=True, text=True, timeout=60, stdin=subprocess.DEVNULL)
    return result.returncode, result.stdout + result.stderr


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    token = Path(glob.glob(str(HOME / '.config/paperboat/credentials/secrets/profile-*-access.secret'))[0]).read_text().strip()
    server = json.loads((HOME / '.config/paperboat/config.json').read_text())['server_url']
    plan = api('/v1/billing/workspaces/personal', token, server)
    if plan['state'] != 'free' or plan['seats_bought'] != 0:
        raise RuntimeError('account is not a plain Free account: state=%s seats=%s' % (plan['state'], plan['seats_bought']))
    if plan['allowance_bytes'] <= 0 or plan['usage_bytes'] >= plan['allowance_bytes'] or not plan['allowance_period_end']:
        raise RuntimeError('Free allowance is missing or already exhausted')
    machines = json.loads(run('machine', 'list', '--json')[1])['machines']
    me = next((m for m in machines if m['alias'] == args.target), None)
    if me is None or me['state'] != 'online' or 'terminal' not in me['permissions'] or 'exec' not in me['permissions']:
        raise RuntimeError('paired machine is not online with terminal and exec on the Free plan')
    for attempt in range(6):  # native path can need a moment after a daemon restart
        code, out = run('exec', args.target, '--', 'true')
        if code == 0:
            break
        time.sleep(15)
    if code:
        raise RuntimeError('exec failed on the Free plan: ' + out[-160:])
    # No subscription, trial or payment gate anywhere in what the CLI recorded locally.
    gate = 0
    for path in glob.glob(str(HOME / '.local/state/paperboat/diagnostics/*.ndjson')):
        for line in open(path, errors='replace'):
            lowered = line.lower()
            if 'machine_plan_inactive' in lowered or 'subscription_required' in lowered or '"http_status":"402"' in lowered.replace(' ', ''):
                gate += 1
    if gate:
        raise RuntimeError('%d local diagnostics events mention a plan gate (402/machine_plan_inactive/subscription_required)' % gate)
    print(json.dumps({'check': 'free-plan', 'target': args.target, 'status': 'PASS', 'state': plan['state'],
                      'allowance_bytes': plan['allowance_bytes'], 'usage_bytes': plan['usage_bytes'],
                      'period_end': plan['allowance_period_end'][:10], 'online_with_terminal_exec': True,
                      'no_plan_gate_events': True}), flush=True)


if __name__ == '__main__':
    main()
