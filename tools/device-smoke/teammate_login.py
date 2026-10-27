#!/usr/bin/env python3
"""Container side: sign the controlled teammate profile in with an API token read from stdin (never saved).

An already signed-in profile prints 'signed-in' and reads nothing. Exits 0 only when the profile is signed in.
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
        print('signed-in')
        return 0
    return subprocess.run([PB, '--config', str(config), 'login', '--api-token-file', '-'], env=env, stdin=sys.stdin).returncode


if __name__ == '__main__':
    sys.exit(main())
