#!/usr/bin/env python3
"""Coordinate two independently signed-in clients for the terminal replay fixture."""
import argparse
import json
from pathlib import Path
import shlex
import sys
import time


def wait_job(job, phase, expected_state):
    deadline = time.monotonic() + (180 if phase else 420)
    while time.monotonic() < deadline:
        if phase:
            try:
                with job.with_suffix('.out').open() as output:
                    value = json.loads(output.readline(8192))
                if value.get('state') == expected_state:
                    print(json.dumps(value))
                    return
            except (FileNotFoundError, ValueError):
                pass
        if job.with_suffix('.end').is_file():
            if phase:
                raise RuntimeError('creator ended before the cross-device handoff; inspect its job output')
            with job.with_suffix('.out').open() as output:
                result = output.read((2 << 20) + 1)
            if len(result) > 2 << 20:
                raise RuntimeError('creator output exceeded its private receipt bound')
            sys.stdout.write(result)
            return
        time.sleep(0.1)
    raise RuntimeError('terminal creator did not reach its bounded handoff or cleanup')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['wait', 'finish', 'command', 'signal'])
    parser.add_argument('--job', type=Path)
    parser.add_argument('--expected-state', choices=['terminal_attach_required', 'preview_protocol_required', 'inbox_sender_required'],
                        default='terminal_attach_required')
    parser.add_argument('--phase')
    parser.add_argument('--script')
    parser.add_argument('--success', choices=['0', '1'])
    args = parser.parse_args()
    if args.action in ('wait', 'finish'):
        if args.job is None:
            parser.error('wait/finish require the exact creator job path')
        wait_job(args.job, args.action == 'wait', args.expected_state)
        return
    if not args.phase:
        parser.error('controller command/signal requires the exact creator phase')
    phase = json.loads(args.phase)
    if phase['state'] != args.expected_state:
        parser.error('unexpected creator phase')
    if args.action == 'command':
        if not args.script or args.expected_state != 'terminal_attach_required':
            parser.error('command requires the staged terminal controller script')
        print(shlex.join(['python3', args.script, phase['target'], '--check', 'resume',
                          '--session-id', phase['session_id'], '--expected-state', phase['expected_state'],
                          '--expected-account', phase['expected_account']]))
    else:
        if args.success is None:
            parser.error('signal requires the actual controller result')
        path = phase['gate' if args.success == '1' else 'failure_gate']
        writer = ('import os,sys; fd=os.open(sys.argv[1],os.O_WRONLY|os.O_CREAT|os.O_EXCL|'
                  'os.O_NOFOLLOW,0o600); os.close(fd)')
        print(shlex.join(['python3', '-c', writer, path]))


if __name__ == '__main__':
    main()
