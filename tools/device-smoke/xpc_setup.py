#!/usr/bin/env python3
"""The cross-OS fleet of the X-20..X-44 rows: one Linux container, one hosted Mac and one hosted Windows runner of ONE throwaway account.

  xpc_setup.py up BUILD_DIR    install the build on L, M and W, sign the Linux container in with an API token, pair all three
  xpc_setup.py revoke NAME...  revoke our records of replaced machines (a hosted runner that expired)
  xpc_setup.py status          pb version and machine state per device
  xpc_setup.py down            revoke the three machines by exact id and uninstall

Environment: XPLAT_PREFIX (machine names `<prefix>-l|-m|-w`), XPLAT_L_HOST/XPLAT_L_CT (Docker host and container with a systemd
`tester` user), XPLAT_MAC_SSH=macpool-N, SMOKE_WINDOWS_SSH=winpool-N, XPLAT_CONTROLLER=L, and for the sign-in ORIGIN, STATE (console
session of the throwaway account, mode 600), PW_MODULE, PW_CHROMIUM (as mint_api_token.mjs). The devices are driven by
xplat_fleet.py. BUILD_DIR holds pb-linux-<arch>, pb-darwin-arm64 and pb-windows-amd64.exe. Nothing secret is printed.
"""
import json
import os
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).parent
sys.path.insert(0, str(HERE))
import xplat_fleet as fleet  # noqa: E402


def api_token():
    done = subprocess.run(['node', str(HERE / 'mint_api_token.mjs'), 'xpc-controller'], capture_output=True, timeout=180)
    assert done.returncode == 0 and done.stdout, 'api token: ' + done.stderr.decode()[-200:]
    return done.stdout


def sign_in_controller(dev):
    shown = fleet.json_pb(dev, 'config', 'show')
    dev.write(dev.home + '/.config/paperboat/config.json', json.dumps({'server_url': shown['server_url'], 'auth': {'allow_file_fallback': True}}).encode())
    done = dev.pb('login', '--api-token-file', '-', '--json', input=api_token())
    assert json.loads(done.text).get('ok'), 'controller login failed: ' + done.text[-200:] + done.errtext[-200:]


def machines():
    done = fleet.controller_pb('machine', 'list', '--json')
    return {m['alias']: m for m in json.loads(done.text)['machines']}


def revoke(names):
    """Revoke our records of these machine names by exact id (a replaced runner leaves its record behind)."""
    known = machines()
    for name in names:
        if name in known:
            token = json.loads(fleet.controller_pb('machine', 'revoke', known[name]['id'], '--json').text)['data']['confirmation_token']
            fleet.controller_pb('machine', 'revoke', known[name]['id'], '--json', '--confirm', token)
            print('revoked', name, known[name]['id'], flush=True)


def up(build, keys='L,M,W'):
    devices = {k: d for k, d in fleet.fleet().items() if k in keys.split(',')}
    binaries = {'L': 'pb-linux-' + os.environ.get('XPC_L_ARCH', 'arm64'), 'M': 'pb-darwin-arm64', 'W': 'pb-windows-amd64.exe'}
    for key, dev in devices.items():
        fleet.install(dev, Path(build) / binaries[key])
        print(key, 'installed', flush=True)
    if 'L' in devices:
        sign_in_controller(devices['L'])
    for key, kind in (('L', 'unix'), ('M', 'unix'), ('W', 'windows')):
        if key in devices:
            print(key, 'paired', fleet.pair(devices[key], kind), flush=True)


def down():
    known = machines()
    for dev in fleet.fleet().values():
        if dev.name in known:
            mid = known[dev.name]['id']
            token = json.loads(fleet.controller_pb('machine', 'revoke', mid, '--json').text)['data']['confirmation_token']
            fleet.controller_pb('machine', 'revoke', mid, '--json', '--confirm', token)
            print('revoked', dev.name, mid)
    for dev in fleet.fleet().values():
        fleet.uninstall(dev)


def status():
    known = machines()
    for key, dev in fleet.fleet().items():
        done = dev.pb('version')
        print(key, dev.name, (done.text.strip().splitlines() or ['?'])[0], known.get(dev.name, {}).get('online', 'unpaired'))


if __name__ == '__main__':
    action = sys.argv[1] if len(sys.argv) > 1 else 'status'
    {'up': lambda: up(*sys.argv[2:4]), 'down': down, 'status': status, 'revoke': lambda: revoke(sys.argv[2:])}[action]()
