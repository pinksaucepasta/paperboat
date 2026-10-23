#!/usr/bin/env python3
"""`pb wait` returns once the target is reachable, and a renamed machine appears in shell completion and
as an SSH alias within 30 s (inventory refreshes on server changes), then the original name returns."""
import argparse
import json
from pathlib import Path
import subprocess
import time
import urllib.request
import uuid

from install_lifecycle import registration
from pairing_lifecycle import runtime_root

PB = Path.home() / '.local/bin/pb'
BUDGET = 30.0


def pb(*args, timeout=90):
    return subprocess.run([str(PB), *args], capture_output=True, text=True, timeout=timeout, stdin=subprocess.DEVNULL)


def completes(name):
    result = pb('__complete', 'exec', '', timeout=15)
    return any(line.split('\t', 1)[0] == name for line in result.stdout.splitlines())


def resolves(host):
    """Every name under the local access domain resolves to the local proxy, so the alias counts only
    when plain OpenSSH reaches the machine through it."""
    try:
        result = subprocess.run(['ssh', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=5', host, 'true'],
                                capture_output=True, timeout=20, stdin=subprocess.DEVNULL)
    except subprocess.TimeoutExpired:
        return False
    return result.returncode == 0


def visible_within(name, domain, budget):
    started = time.monotonic()
    while time.monotonic() - started < budget:
        if completes(name) and resolves(name + '.' + domain):
            return time.monotonic() - started
        time.sleep(1)
    return None


def rename(current, new):
    result = pb('machine', 'rename', current, new, '--json')
    if result.returncode != 0:
        raise RuntimeError('pb machine rename %s failed' % current)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    before = registration()
    binding = json.loads((runtime_root() / 'machine-registration.json').read_text())
    with urllib.request.urlopen(binding['server_url'].rstrip('/') + '/v1/client-configuration', timeout=15) as response:
        domain = json.load(response)['data']['local_access_base_domain']
    started = time.monotonic()
    waited = pb('wait', args.target, '--for', 'transport', '--timeout', '60s', '--json', timeout=90)
    if waited.returncode != 0:
        raise RuntimeError('pb wait did not report the target reachable within 60 s')
    wait_seconds = time.monotonic() - started
    renamed = args.target + '-r' + uuid.uuid4().hex[:6]
    rename(args.target, renamed)
    try:
        appeared = visible_within(renamed, domain, BUDGET)
        if appeared is None:
            raise RuntimeError('the renamed machine did not appear in completion and as an SSH alias within 30 s')
    finally:
        rename(renamed, args.target)
    restored = visible_within(args.target, domain, BUDGET)
    if restored is None:
        raise RuntimeError('the original name did not come back in completion and as an SSH alias within 30 s')
    if registration() != before:
        raise RuntimeError('inventory freshness changed the enrolled identity')
    print(json.dumps({'check': 'inventory-freshness', 'target': args.target, 'status': 'PASS', 'wait_s': round(wait_seconds, 1),
                      'rename_visible_s': round(appeared, 1), 'restore_visible_s': round(restored, 1), 'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
