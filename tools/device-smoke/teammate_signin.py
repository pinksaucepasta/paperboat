#!/usr/bin/env python3
"""Controller side: sign the controlled teammate profile of a container in through the console approval page.

The member's console session is a Playwright storage state (mode 600, never printed). Idempotent: a profile that
is already signed in is left alone.
Usage: teammate_signin.py HOST CONTAINER USER   env: STATE, PW_CHROMIUM, [PW_MODULE]
"""
import os
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent


def main():
    host, container, user = sys.argv[1:4]
    docker = ['ssh', host, 'docker', 'exec', '-i', '-u', user, '-e', 'HOME=/home/' + user, '-w', '/tmp/pbts', container]
    stage = subprocess.run(['ssh', host, 'docker exec -i -u %s %s sh -c "rm -rf /tmp/pbts && mkdir /tmp/pbts && tar xzf - -C /tmp/pbts"' % (user, container)],
                           input=subprocess.run(['tar', 'czf', '-', '-C', str(HERE), 'teammate_login.py', 'teammate_profile.py'], capture_output=True, check=True).stdout)
    if stage.returncode:
        raise RuntimeError('could not stage the teammate sign-in script')
    login = subprocess.Popen(docker + ['python3', 'teammate_login.py'], stdout=subprocess.PIPE, text=True)
    first = login.stdout.readline().strip()
    if first == 'signed-in':
        print('teammate already signed in')
        return 0
    if not first.startswith('https://'):
        raise RuntimeError('teammate sign-in printed no approval link')
    subprocess.run(['node', str(HERE / 'approve_cli_login.mjs'), first], check=True, timeout=180, env=os.environ)
    if login.wait(timeout=120):
        raise RuntimeError('teammate sign-in did not complete')
    print('teammate signed in')
    return 0


if __name__ == '__main__':
    sys.exit(main())
