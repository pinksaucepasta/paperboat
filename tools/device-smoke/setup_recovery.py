#!/usr/bin/env python3
"""Interrupt authenticated setup and recover the exact existing machine identity."""
import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time
from urllib.parse import urlparse

from pairing_fault import PairingFaultProxy
from pairing_lifecycle import interrupt_pair, invoke, runtime_root


def binding(record):
    return {key: record[key] for key in ('machine_id', 'environment_id',
            'public_identity_key', 'public_key_id', 'installation_generation',
            'account_id', 'inbox_path')}


def interrupt_admitted(binary, arguments, root):
    process = subprocess.Popen([str(binary), *arguments], stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE, stdin=subprocess.DEVNULL,
                               start_new_session=True)
    try:
        deadline = time.monotonic() + 30
        while process.poll() is None and time.monotonic() < deadline:
            try:
                record = json.loads((root / 'bootstrap-resume.json').read_text())
            except (FileNotFoundError, ValueError):
                record = {}
            if record.get('runtime_enrolled') and not record.get('runtime_ready'):
                os.kill(process.pid, signal.SIGINT)
                process.communicate(timeout=90)
                if process.returncode == 0:
                    raise RuntimeError('SIGINT did not interrupt admitted setup')
                return
            time.sleep(0.005)
        raise RuntimeError('admitted setup checkpoint was not observed')
    finally:
        if process.poll() is None:
            os.killpg(process.pid, signal.SIGKILL)
            process.communicate(timeout=10)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('fault', choices=['sigint', 'network'])
    parser.add_argument('--password-stdin', action='store_true')
    args = parser.parse_args()
    password = sys.stdin.readline().rstrip('\n') if args.password_stdin else None
    os.umask(0o077)
    binary = Path.home() / '.local/bin/pb'
    root = runtime_root()
    registration = root / 'machine-registration.json'
    before = json.loads(registration.read_text())
    catalog = invoke(binary, 'machine', 'list', '--json')
    machine = next(item for item in catalog['machines'] if item['id'] == before['machine_id'])
    arguments = ['setup', '--name', machine['alias'], '--json']
    proxy = None
    environment = None
    started = time.monotonic()
    failure = None
    try:
        if args.fault == 'sigint':
            interrupt_admitted(binary, arguments, root)
        else:
            origin = urlparse(before['server_url'])
            if origin.scheme != 'https':
                raise RuntimeError('network fault requires end-to-end TLS')
            proxy = PairingFaultProxy(origin.hostname, origin.port or 443)
            environment = interrupt_pair(binary, arguments, root, 'network', proxy)
    except Exception as error:
        failure = error
    try:
        # Recover even when fault observation failed; preserve that primary failure.
        # Exact command and protected journal, without enrollment or manual edits.
        invoke(binary, *arguments, env=environment, raw=True, terminal=True, password=password)
    except Exception as error:
        if failure is not None:
            raise failure from error
        raise
    finally:
        if proxy is not None:
            proxy.close()
    if failure is not None:
        raise failure
    if binding(json.loads(registration.read_text())) != binding(before):
        raise RuntimeError('recovery changed original enrollment or Inbox')
    if (root / 'bootstrap-resume.json').exists():
        raise RuntimeError('completed recovery retained unfinished journal')
    doctor = invoke(binary, 'doctor', '--json')
    if doctor.get('overall') != 'healthy':
        raise RuntimeError('recovered machine is not healthy')
    print(json.dumps({'check': 'setup-recovery-' + args.fault, 'status': 'PASS',
                      'same_identity': True, 'seconds': round(time.monotonic()-started, 2)}))


if __name__ == '__main__':
    main()
