#!/usr/bin/env python3
"""Hold one managed SSH session for over an hour and require its final marker and exit status (F5-02)."""
import argparse
import json
from pathlib import Path
import subprocess
import uuid

from install_lifecycle import registration


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--seconds', type=int, default=3601)
    args = parser.parse_args()
    before = registration()
    marker = 'PB_SSH_LONG_' + uuid.uuid4().hex
    # The session prints once a minute so idle timeouts on any hop would show, then the marker at the end.
    command = (f"i=0; while [ $i -lt {args.seconds} ]; do sleep 60; i=$((i+60)); printf 't%s\\n' $i; done; "
               f"printf '%s\\n' {marker}")
    result = subprocess.run([str(Path.home() / '.local/bin/pb'), 'ssh', args.target, '--', command],
                            capture_output=True, timeout=args.seconds + 300)
    if result.returncode != 0 or not result.stdout.decode(errors='replace').rstrip().endswith(marker):
        raise RuntimeError(f'the long SSH session ended with exit {result.returncode} before its final marker')
    if registration() != before:
        raise RuntimeError('SSH changed the enrolled identity')
    print(json.dumps({'check': 'ssh-long', 'target': args.target, 'status': 'PASS', 'seconds': args.seconds,
                      'final_marker': True, 'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
