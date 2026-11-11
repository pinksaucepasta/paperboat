#!/usr/bin/env python3
"""Controller side: sign the controlled teammate profile of a container in with an API token of the member account.

The member's console session is a Playwright storage state (mode 600, never printed); the token is minted from it,
valid one day, and goes straight into the container's `pb login --api-token-file -`. Idempotent: a profile that is
already signed in is left alone.
Usage: teammate_signin.py HOST CONTAINER USER   env: ORIGIN, STATE, PW_CHROMIUM, [PW_MODULE]

Windows device (the profile is %USERPROFILE%\\.pb-smoke\\teammate; only pb.exe runs there, through winhost):
       teammate_signin.py --windows-install-json FILE   env: VICTUS_PASS, ORIGIN, STATE, PW_CHROMIUM, [PW_MODULE]
"""
import json
import os
import shlex
import subprocess
import sys
import time
from pathlib import Path

import install_path
import winhost
from pairing_lifecycle import invoke
from teammate_profile import environment, prepare, profile_root

HERE = Path(__file__).resolve().parent


def mint():
    return subprocess.run(['node', str(HERE / 'mint_api_token.mjs'), 'smoke-teammate-%d' % int(time.time())],
                          capture_output=True, check=True, timeout=120).stdout


def device_signin(mint_token):
    """Sign the teammate profile of the Windows device in; returns False when it already was.

    The XDG_* overlay is what keeps the device's own login untouched: with XDG_STATE_HOME moved, pb.exe finds no
    machine registration, so the token session is not bound to the device's machine and its daemon is left alone;
    with XDG_CONFIG_HOME moved, credentials-location.json of the device account is not rewritten."""
    pb, root = winhost.pb_path(), profile_root()
    config = root / 'config.json'
    if not config.exists():  # pb writes its config.json only once a setting changes: ask pb what it uses
        shown = invoke(pb, 'config', 'show', '--json', timeout=40)
        prepare(root, {'server_url': shown['server_url'],
                       'auth': {'allow_file_fallback': bool(shown.get('auth_file_fallback'))}})
    env = environment(root)
    status = pb.run_remote(['--config', str(config), 'auth', 'status', '--json'], input='', env=env, timeout=40)
    try:
        if status.returncode == 0 and json.loads(status.stdout or '{}').get('signed_in'):
            return False
    except ValueError:
        pass
    done = pb.run_remote(['--config', str(config), 'login', '--api-token-file', '-'], input=mint_token(), env=env,
                         timeout=90)
    if done.returncode:
        raise RuntimeError('teammate sign-in failed: ' + done.stdout[-200:] + done.stderr[-200:])
    return True


def main():
    if sys.argv[1:2] == ['--windows-install-json']:
        os.environ['PB_SMOKE_WINDOWS_PB'] = install_path.windows_executable(json.loads(Path(sys.argv[2]).read_text()))
        print('teammate signed in' if device_signin(mint) else 'teammate already signed in')
        return 0
    host, container, user = sys.argv[1:4]
    exec_in = 'ssh %s docker exec -i -u %s -e HOME=/home/%s -w /tmp/pbts %s ' % (host, user, user, container)
    tarball = subprocess.run(['tar', 'czf', '-', '-C', str(HERE), 'teammate_login.py', 'teammate_profile.py', 'winhost.py'], capture_output=True, check=True).stdout
    stage = 'sh -c ' + shlex.quote('rm -rf /tmp/pbts && mkdir /tmp/pbts && tar xzf - -C /tmp/pbts')
    subprocess.run(exec_in.replace('-w /tmp/pbts ', '') + shlex.quote(stage), shell=True, input=tarball, check=True)
    login = shlex.quote('sh -c ' + shlex.quote('XDG_RUNTIME_DIR=/run/user/$(id -u) python3 teammate_login.py'))
    probe = subprocess.run(exec_in + login, shell=True, input=b'', capture_output=True)
    if probe.stdout.strip() == b'signed-in':
        print('teammate already signed in')
        return 0
    token = mint()
    done = subprocess.run(exec_in + login, shell=True, input=token, capture_output=True)
    if done.returncode:
        raise RuntimeError('teammate sign-in failed: ' + done.stdout.decode()[-200:] + done.stderr.decode()[-200:])
    print('teammate signed in')
    return 0


if __name__ == '__main__':
    sys.exit(main())
