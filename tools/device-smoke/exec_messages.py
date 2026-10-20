#!/usr/bin/env python3
"""F4-04: exec to an offline or unknown machine says so before any remote effect.

Uses one machine of the signed-in account that is not online, and a name no machine has.
Nothing is changed: both are rejected before a descriptor or a peer stream is requested.
"""
import json
from pathlib import Path
import subprocess
import uuid

PB = Path.home() / '.local/bin/pb'


def run(*args):
    result = subprocess.run([str(PB), *args], capture_output=True, text=True, timeout=90)
    return result.returncode, result.stdout, result.stderr


def failure(code, out, err, error_code, words):
    if code == 0:
        raise RuntimeError(error_code + ': exec succeeded')
    try:
        event = json.loads(out)
    except ValueError:
        raise RuntimeError(error_code + ': no JSON event') from None
    if event.get('error_code') != error_code or event.get('changed') is not False:
        raise RuntimeError('%s: unexpected outcome %s' % (error_code, event.get('error_code')))
    if not all(word in event.get('detail', '') for word in words):
        raise RuntimeError(error_code + ': message does not give the next step')
    return event


def main():
    machines = json.loads(run('machine', 'list', '--json')[1])['machines']
    offline = next((m for m in machines if not m['online']), None)
    if offline is None:
        raise RuntimeError('the account has no offline machine to check against')
    code, out, err = run('exec', offline['id'], '--', 'true')
    if code == 0 or 'offline' not in err or 'retry' not in err:
        raise RuntimeError('offline exec does not say the machine is offline')
    code, out, err = run('exec', offline['id'], '--json', '--', 'true')
    failure(code, out, err, 'machine_offline', ('offline', 'retry'))
    missing = 'no-such-' + uuid.uuid4().hex[:8]
    code, out, err = run('exec', missing, '--', 'true')
    if code == 0 or 'No machine has that name or ID' not in err or 'pb machine list' not in err:
        raise RuntimeError('unknown machine exec does not say it was not found')
    code, out, err = run('exec', missing, '--json', '--', 'true')
    failure(code, out, err, 'machine_not_found', ('No machine has that name or ID', 'pb machine list'))
    print(json.dumps({'check': 'exec-messages', 'status': 'PASS', 'offline_message': True,
                      'unknown_machine_message': True, 'nothing_changed': True}), flush=True)


if __name__ == '__main__':
    main()
