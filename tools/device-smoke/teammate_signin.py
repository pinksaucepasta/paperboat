#!/usr/bin/env python3
"""Controller side: sign the controlled teammate profile of a container in with an API token of the member account.

The member's console session is a Playwright storage state (mode 600, never printed); the token is minted from it,
valid one day, and goes straight into the container's `pb login --api-token-file -`. Idempotent: a profile that is
already signed in is left alone.
Usage: teammate_signin.py HOST CONTAINER USER   env: ORIGIN, STATE, PW_CHROMIUM, [PW_MODULE]
"""
import shlex
import subprocess
import sys
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent


def main():
    host, container, user = sys.argv[1:4]
    exec_in = 'ssh %s docker exec -i -u %s -e HOME=/home/%s -w /tmp/pbts %s ' % (host, user, user, container)
    tarball = subprocess.run(['tar', 'czf', '-', '-C', str(HERE), 'teammate_login.py', 'teammate_profile.py'], capture_output=True, check=True).stdout
    stage = 'sh -c ' + shlex.quote('rm -rf /tmp/pbts && mkdir /tmp/pbts && tar xzf - -C /tmp/pbts')
    subprocess.run(exec_in.replace('-w /tmp/pbts ', '') + shlex.quote(stage), shell=True, input=tarball, check=True)
    login = shlex.quote('sh -c ' + shlex.quote('XDG_RUNTIME_DIR=/run/user/$(id -u) python3 teammate_login.py'))
    probe = subprocess.run(exec_in + login, shell=True, input=b'', capture_output=True)
    if probe.stdout.strip() == b'signed-in':
        print('teammate already signed in')
        return 0
    token = subprocess.run(['node', str(HERE / 'mint_api_token.mjs'), 'smoke-teammate-%d' % int(time.time())], capture_output=True, check=True, timeout=120).stdout
    done = subprocess.run(exec_in + login, shell=True, input=token, capture_output=True)
    if done.returncode:
        raise RuntimeError('teammate sign-in failed: ' + done.stdout.decode()[-200:] + done.stderr.decode()[-200:])
    print('teammate signed in')
    return 0


if __name__ == '__main__':
    sys.exit(main())
