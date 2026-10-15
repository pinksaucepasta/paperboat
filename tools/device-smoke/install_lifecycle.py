#!/usr/bin/env python3
"""Run bounded installation checks as the enrolled Unix user; emit no identities."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import time


def registration():
    relative = ('.local/state/paperboat/runtime' if os.uname().sysname == 'Linux'
                else 'Library/Application Support/Paperboat/runtime')
    record = json.loads((Path.home() / relative / 'machine-registration.json').read_text())
    return {key: record[key] for key in ('machine_id', 'environment_id',
            'public_identity_key', 'public_key_id', 'installation_generation', 'account_id')}


def command(pb, *args, timeout=45, env=None):
    result = subprocess.run([str(pb), *args], capture_output=True, text=True,
                            timeout=timeout, env=env, stdin=subprocess.DEVNULL)
    try:
        receipt = json.loads(result.stdout)
    except ValueError:
        raise RuntimeError('command did not return a JSON receipt') from None
    if result.returncode or receipt.get('ok') is not True:
        raise RuntimeError('command failed; private output withheld')
    return receipt.get('data', {})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('check', choices=['reinstall', 'pair-refusal', 'service'])
    args = parser.parse_args()
    pb = Path.home() / '.local/bin/pb'
    before = registration()
    started = time.monotonic()
    if args.check == 'reinstall':
        previous = os.umask(0o077)
        try:
            receipt = command(pb, 'install', '--json', timeout=180,
                              env=dict(os.environ, PATH='/usr/bin:/bin:/usr/sbin:/sbin'))
        finally:
            os.umask(previous)
        if receipt.get('enrollment') != 'unchanged':
            raise RuntimeError('install did not confirm preserved enrollment')
        if receipt.get('automatic_update_checks') is not False:
            raise RuntimeError('custom build enabled automatic updates')
    elif args.check == 'pair-refusal':
        result = subprocess.run([str(pb), 'pair', '--json'], capture_output=True,
                                text=True, timeout=45, stdin=subprocess.DEVNULL)
        receipt = json.loads(result.stdout)
        message = receipt.get('error', {}).get('message', '')
        if result.returncode == 0 or 'pb setup --name <machine-alias>' not in message:
            raise RuntimeError('existing enrollment refusal lost its recovery command')
    else:
        try:
            for operation in ['install', 'stop', 'start', 'restart', 'uninstall', 'install']:
                receipt = command(pb, 'service', operation, '--json')
                if receipt.get('completed') is not True:
                    raise RuntimeError('service operation was not acknowledged')
                state = command(pb, 'service', 'status', '--json').get('status')
                expected = 'running' if operation in ('install', 'start', 'restart') else None
                if expected and state != expected or not expected and state == 'running':
                    raise RuntimeError('service status does not match completed operation')
        finally:
            # Leave a working daemon even when an intermediate assertion fails.
            command(pb, 'service', 'install', '--json')
    if registration() != before:
        raise RuntimeError('operation changed the protected enrollment')
    if command(pb, 'service', 'status', '--json').get('status') != 'running':
        raise RuntimeError('local daemon was not left running')
    print(json.dumps({'check': args.check, 'status': 'PASS',
                      'seconds': round(time.monotonic() - started, 1),
                      'enrollment_preserved': True}))


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print(json.dumps({'status': 'FAIL', 'reason': str(error)}))
        raise SystemExit(1)
