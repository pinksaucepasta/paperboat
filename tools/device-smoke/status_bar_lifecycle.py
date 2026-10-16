#!/usr/bin/env python3
"""Exercise persistent status-bar preferences and an attached terminal display."""
import argparse
import json
from pathlib import Path
import tempfile

from pairing_lifecycle import invoke
from terminal_lifecycle import qualify


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    binary = Path.home() / '.local/bin/pb'
    with tempfile.TemporaryDirectory(prefix='paperboat-status-bar-') as directory:
        config = str(Path(directory) / 'config.json')
        prefix = ('--config', config, 'config', 'status-bar')
        invoke(binary, *prefix, raw=True)
        original = invoke(binary, *prefix, 'show', '--json')
        for key, value in [('mode', 'on'), ('theme', 'mono'), ('privacy', 'true'),
                           ('fullscreen', 'hide'), ('notice-seconds', '7')]:
            invoke(binary, *prefix, 'set', key, value, '--json')
        observed = invoke(binary, *prefix, 'show', '--json')
        for key, value in [('mode', 'on'), ('theme', 'mono'), ('privacy', True),
                           ('fullscreen', 'hide'), ('notice_seconds', 7)]:
            if observed.get(key) != value:
                raise RuntimeError('status-bar preference did not persist: ' + key)
        preview = invoke(binary, *prefix, 'preview', '--width', '80', raw=True)
        if not preview.strip():
            raise RuntimeError('status-bar preview rendered no display')
        invoke(binary, *prefix, 'reset', '--json')
        if invoke(binary, *prefix, 'show', '--json') != original:
            raise RuntimeError('status-bar reset did not restore product defaults')
    qualify(binary, args.target, 'main', 'htop', status_bar='on')
    print(json.dumps({'check': 'status-bar', 'target': args.target, 'status': 'PASS',
                      'preferences_persist': True, 'preview': True, 'reset': True,
                      'live_reserved_row': True}), flush=True)


if __name__ == '__main__':
    main()
