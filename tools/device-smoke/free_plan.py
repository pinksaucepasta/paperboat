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
    state = HOME / ('Library/Application Support/Paperboat/state' if cli_credential.DARWIN else '.local/state/paperboat')
    gate = gate_events()
    if gate:
        raise RuntimeError('%d local diagnostics events mention a plan gate (402/machine_plan_inactive/subscription_required)' % gate)
    print(json.dumps({'check': 'free-plan', 'target': args.target, 'status': 'PASS', 'state': plan['state'],
                      'allowance_bytes': plan['allowance_bytes'], 'usage_bytes': plan['usage_bytes'],
                      'period_end': plan['allowance_period_end'][:10], 'online_with_terminal_exec': True,
                      'no_plan_gate_events': True}), flush=True)


if __name__ == '__main__':
    main()
