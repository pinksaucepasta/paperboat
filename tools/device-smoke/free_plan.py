#!/usr/bin/env python3
"""F13-01 on a Free account: the paired machine works with no subscription or trial, on the allowance model.

Reads the CLI's own access credential in memory (never printed). Usage: free_plan.py TARGET
On the Windows device (PB_SMOKE_WINDOWS_PB, see winhost.py) the CLI token is DPAPI-protected, so the plan is read
with a one-day API token of the same account, made by pb.exe for this run and revoked at its end.
"""
import argparse
import contextlib
import glob
import json
import os
from pathlib import Path
import secrets
import subprocess
import time
import urllib.request

import winhost

HOME = Path.home()
PB = winhost.pb_path()
GATES = ('machine_plan_inactive', 'subscription_required', '"http_status":"402"')


def api(path, token, server):
    request = urllib.request.Request(server + path, headers={'Authorization': 'Bearer ' + token,
                                                             'X-Paperboat-Workspace': 'personal'})
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.loads(response.read())['data']


def run(*args):
    result = winhost.pb_text(PB, args, timeout=60, stdin=subprocess.DEVNULL)
    return result.returncode, result.stdout + result.stderr


@contextlib.contextmanager
def credential():
    """(access token, server URL) of the signed-in account."""
    device = winhost.device()
    if not device:
        import cli_credential
        yield cli_credential.access_token(), cli_credential.server_url()
        return
    name = 'free-plan-' + secrets.token_hex(4)
    created = winhost.pb_text(PB, ['api-token', 'create', name, '--workspace', 'personal', '--expires', '1d', '--json'], timeout=60)
    try:
        value = json.loads(created.stdout)
        server = json.loads((device.runtime_dir() / 'machine-registration.json').read_text())['server_url']
        yield value.get('data', value)['secret'], server
    finally:
        run('api-token', 'revoke', name, '--workspace', 'personal', '--yes')


def gate_events():
    """How many local diagnostics events mention a plan gate."""
    device = winhost.device()
    if device:
        directory = winhost.RemotePath(device, device.env('LOCALAPPDATA')) / 'Paperboat' / 'state' / 'diagnostics'
        return sum(device.count_in_files(directory, '*.ndjson', text) for text in GATES + ('"http_status": "402"',))
    import cli_credential
    gate = 0
    state = HOME / ('Library/Application Support/Paperboat/state' if cli_credential.DARWIN else '.local/state/paperboat')
    for path in glob.glob(str(state / 'diagnostics/*.ndjson')):
        for line in open(path, errors='replace'):
            lowered = line.lower()
            if 'machine_plan_inactive' in lowered or 'subscription_required' in lowered or '"http_status":"402"' in lowered.replace(' ', ''):
                gate += 1
    return gate


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    with credential() as (token, server):
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
        code, out = run('exec', args.target, '--', *winhost.true_argv())
        if code == 0:
            break
        time.sleep(15)
    if code:
        raise RuntimeError('exec failed on the Free plan: ' + out[-160:])
    # No subscription, trial or payment gate anywhere in what the CLI recorded locally.
    gate = gate_events()
    if gate:
        raise RuntimeError('%d local diagnostics events mention a plan gate (402/machine_plan_inactive/subscription_required)' % gate)
    print(json.dumps({'check': 'free-plan', 'target': args.target, 'status': 'PASS', 'state': plan['state'],
                      'allowance_bytes': plan['allowance_bytes'], 'usage_bytes': plan['usage_bytes'],
                      'period_end': plan['allowance_period_end'][:10], 'online_with_terminal_exec': True,
                      'no_plan_gate_events': True}), flush=True)


if __name__ == '__main__':
    main()
