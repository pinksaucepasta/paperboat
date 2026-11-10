#!/usr/bin/env python3
"""Hold one `pb exec` for over 30 minutes and require its final output and exit status (F4-02)."""
import argparse
import json
import uuid

from install_lifecycle import registration
import windows_target
import winhost


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--seconds', type=int, default=1801)
    args = parser.parse_args()
    binary = winhost.pb_path()
    before = registration()
    marker = 'PB_EXEC_LONG_' + uuid.uuid4().hex
    command = ['sh', '-c', f'sleep {args.seconds}; echo {marker}']
    if windows_target.is_windows(binary, args.target):
        command = windows_target.ps_argv(f"Start-Sleep -Seconds {args.seconds}; '{marker}'")
    result = winhost.run_pb(binary, ['exec', args.target, '--timeout', f'{args.seconds + 300}s', '--', *command],
                            args.seconds + 300)
    if result.returncode != 0 or not result.stdout.decode(errors='replace').rstrip().endswith(marker):
        raise RuntimeError(f'the long exec ended with exit {result.returncode} before its final marker: '
                           + result.stderr.decode(errors='replace')[-300:])
    if registration() != before:
        raise RuntimeError('exec changed the enrolled identity')
    print(json.dumps({'check': 'exec-long', 'target': args.target, 'status': 'PASS', 'seconds': args.seconds,
                      'final_marker': True, 'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
