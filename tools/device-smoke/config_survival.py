#!/usr/bin/env python3
"""F15-04: config set/customize persist through `pb update --from`, then reset restores the originals."""
import json
import subprocess
import sys

from install_lifecycle import registration
from pairing_lifecycle import command_result
import pathlib
import winhost

PB = winhost.pb_path()
HERE = pathlib.Path(__file__).resolve().parent


def phase(name, state):
    result = subprocess.run([sys.executable, str(HERE / 'config_lifecycle.py'), name, str(state)],
                            capture_output=True, text=True, timeout=300)
    if result.returncode:
        raise RuntimeError('config_lifecycle %s failed: %s' % (name, (result.stdout + result.stderr)[-300:]))
    return json.loads(result.stdout.strip().splitlines()[-1])


def main():
    before = registration()
    with winhost.workdir('paperboat-config-survival-') as directory:
        state = directory / 'state'
        state.mkdir(mode=0o700)
        copy = directory / ('pb.exe' if winhost.device() else 'pb')
        winhost.copy_file(str(PB), copy)
        try:
            phase('set', state)
        except Exception:
            # The set phase may have changed settings before failing: put the originals back.
            try:
                phase('verify', state)
            except Exception:
                pass
            raise
        try:
            if command_result(PB, 'update', '--from', str(copy), timeout=300).returncode:
                raise RuntimeError('pb update --from failed')
        finally:
            verified = phase('verify', state)
    if registration()['machine_id'] != before['machine_id']:
        raise RuntimeError('the update changed the enrolled machine')
    print(json.dumps({'check': 'config-survival', 'status': 'PASS', **{k: v for k, v in verified.items() if k.endswith('_survives_update') or k == 'reset_restores'},
                      'enrollment_preserved': True}))


if __name__ == '__main__':
    main()
