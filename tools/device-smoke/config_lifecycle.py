#!/usr/bin/env python3
"""F15-04: config set/show/path, customize shortcuts and defaults, and survival across an update.

Phase `set` records originals under STATE_DIR and applies the changes. Phase `verify`
(run after `pb update --from`) proves they survived, then restores everything.
"""
import argparse
import json
import re
from pathlib import Path
import subprocess
import tempfile

PB = Path.home() / '.local/bin/pb'
SHORTCUT = 'smokeuptime'
PORT = '2201'


def run(*args, timeout=60):
    result = subprocess.run([str(PB), *args], capture_output=True, text=True, timeout=timeout,
                            stdin=subprocess.DEVNULL)
    return result.returncode, result.stdout + result.stderr


def data(*args):
    code, out = run(*args, '--json')
    receipt = json.loads(out)
    if code or receipt.get('ok') is not True:
        raise RuntimeError(' '.join(args[:3]) + ' failed')
    return receipt['data']


def effective():
    code, out = run('config', 'show', '--json')
    if code:
        raise RuntimeError('config show failed')
    return json.loads(out)


def preferences():
    return data('config', 'customize', 'show')['preferences']


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('phase', choices=['set', 'verify'])
    parser.add_argument('state_dir', type=Path)
    args = parser.parse_args()
    state = args.state_dir / 'originals.json'
    if args.phase == 'set':
        args.state_dir.mkdir(mode=0o700, exist_ok=True)
        shown = effective()
        originals = {'port': shown.get('ssh_target_port'), 'preferences': preferences()}
        state.write_text(json.dumps(originals))
        path = run('config', 'path')[1].strip()
        if path != shown['path'] or not Path(path).is_absolute():
            raise RuntimeError('config path and show disagree')
        data('config', 'set', 'ssh-target-port', PORT)
        if str(effective().get('ssh_target_port')) != PORT:
            raise RuntimeError('config set did not change the effective value')
        document = dict(originals['preferences'])
        document['shortcuts'] = {SHORTCUT: {'command': ['exec'], 'args': ['{1}', '--', 'uptime']}}
        document['defaults'] = {'exec': {'timeout': '45s'}}
        with tempfile.TemporaryDirectory(prefix='paperboat-prefs-') as directory:
            candidate = Path(directory) / 'preferences.json'
            candidate.write_text(json.dumps(document))
            data('config', 'customize', 'import', str(candidate))
        explained = run('config', 'customize', 'explain', '--', SHORTCUT, 'machine1')[1]
        if 'exec' not in explained or 'uptime' not in explained:
            raise RuntimeError('shortcut did not expand to the configured command')
        if run('--no-customization', 'config', 'customize', 'explain', '--', SHORTCUT, 'machine1')[0] == 0:
            raise RuntimeError('--no-customization still expanded the shortcut')
        print(json.dumps({'check': 'config-lifecycle', 'phase': 'set', 'status': 'PASS'}), flush=True)
        return
    originals = json.loads(state.read_text())
    try:
        if str(effective().get('ssh_target_port')) != PORT:
            raise RuntimeError('config value did not survive the update')
        prefs = preferences()
        if SHORTCUT not in prefs.get('shortcuts', {}) or prefs.get('defaults') != {'exec': {'timeout': '45s'}}:
            raise RuntimeError('customization did not survive the update')
        explained = run('config', 'customize', 'explain', '--', SHORTCUT, 'machine1')[1]
        if 'uptime' not in explained:
            raise RuntimeError('shortcut stopped expanding after the update')
    finally:
        if originals['port'] is not None:
            run('config', 'set', 'ssh-target-port', str(originals['port']))
        # Reset is a two-step action: the first call only previews and prints a confirmation code.
        preview = run('config', 'customize', 'reset')[1]
        code = re.search(r'--confirm ([A-Z0-9]{6})', preview)
        if code:
            run('config', 'customize', 'reset', '--confirm', code.group(1))
    if preferences() != originals['preferences']:
        raise RuntimeError('reset did not restore the original preferences')
    state.unlink()
    print(json.dumps({'check': 'config-lifecycle', 'phase': 'verify', 'status': 'PASS',
                      'config_set_survives_update': True, 'customization_survives_update': True,
                      'reset_restores': True}), flush=True)


if __name__ == '__main__':
    main()
