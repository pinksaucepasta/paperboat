#!/usr/bin/env python3
"""F15-04: config set/customize persist through `pb update --from`, then reset restores the originals."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

from install_lifecycle import registration

PB = Path.home() / '.local/bin/pb'
HERE = Path(__file__).resolve().parent


def phase(name, state):
    result = subprocess.run([sys.executable, str(HERE / 'config_lifecycle.py'), name, str(state)],
                            capture_output=True, text=True, timeout=300)
    if result.returncode:
        raise RuntimeError('config_lifecycle %s failed: %s' % (name, (result.stdout + result.stderr)[-300:]))
    return json.loads(result.stdout.strip().splitlines()[-1])


def main():
    before = registration()
    with tempfile.TemporaryDirectory(prefix='paperboat-config-survival-', dir=Path.home()) as directory:
        state = Path(directory) / 'state'
        state.mkdir(mode=0o700)
        copy = Path(directory) / 'pb'
        shutil.copy2(PB, copy)
        os.chmod(copy, 0o700)
        phase('set', state)
        try:
            subprocess.run([str(PB), 'update', '--from', str(copy)], check=True, timeout=300, capture_output=True)
        finally:
            verified = phase('verify', state)
    if registration()['machine_id'] != before['machine_id']:
        raise RuntimeError('the update changed the enrolled machine')
    print(json.dumps({'check': 'config-survival', 'status': 'PASS', **{k: v for k, v in verified.items() if k.endswith('_survives_update') or k == 'reset_restores'},
                      'enrollment_preserved': True}))


if __name__ == '__main__':
    main()
