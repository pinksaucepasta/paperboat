#!/usr/bin/env python3
"""Container side: start browser-approval sign-in of the controlled teammate profile and print its approval link.

Prints the link as the only stdout line, then waits for the approval (up to 5 minutes). Exits 0 only when the
profile is signed in. An already signed-in profile prints 'signed-in' and exits at once.
Usage: teammate_login.py   (HOME of the enrolled device user; the profile lives under profile_root())
"""
import json
import subprocess
import sys
from pathlib import Path

from teammate_profile import environment, prepare, profile_root

PB = str(Path.home() / '.local/bin/pb')


def main():
    root = profile_root()
    config = root / 'config.json'
    if not config.exists():
        prepare(root, json.loads((Path.home() / '.config/paperboat/config.json').read_text()))
    env = environment(root)
    for name in ('config', 'data', 'state', 'cache', 'home'):
        (root / name).mkdir(parents=True, exist_ok=True, mode=0o700)
    status = subprocess.run([PB, '--config', str(config), 'auth', 'status', '--json'], env=env, capture_output=True, text=True)
    if status.returncode == 0 and json.loads(status.stdout or '{}').get('signed_in'):
        print('signed-in', flush=True)
        return 0
    login = subprocess.Popen([PB, '--config', str(config), 'login', '--no-browser', '--json'], env=env,
                             stdout=subprocess.PIPE, text=True)
    for line in login.stdout:
        try:
            link = json.loads(line).get('verification_uri_complete')
        except ValueError:
            continue
        if link:
            print(link, flush=True)
            break
    else:
        return login.wait() or 1
    return login.wait(timeout=300)


if __name__ == '__main__':
    sys.exit(main())
