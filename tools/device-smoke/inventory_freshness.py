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
import windows_target
import winhost

PB = winhost.pb_path()
TRUE = ['true']  # what plain ssh runs on the target; cmd.exe on a Windows target has no `true`
BUDGET = 30.0


def pb(*args, timeout=90):
    return winhost.pb_text(PB, args, timeout=timeout, stdin=subprocess.DEVNULL)


def completes(name):
    result = pb('__complete', 'exec', '', timeout=15)
    return any(line.split('\t', 1)[0] == name for line in result.stdout.splitlines())


def ssh_failure(host):
    """None when plain OpenSSH reaches the machine, else its exit code and last lines. Every name under the local
    access domain resolves to the local proxy, so only a connection proves the alias; a failed one does not prove
    the alias is missing (a duplicate host key fails it too)."""
    try:
        if winhost.device():  # the device's own OpenSSH client
            result = winhost.which('ssh').run_remote(['-o', 'BatchMode=yes', '-o', 'ConnectTimeout=5', host, *TRUE], timeout=20)
        else:
            result = subprocess.run(['ssh', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=5', host, *TRUE],
                                    capture_output=True, timeout=20, stdin=subprocess.DEVNULL)
    except subprocess.TimeoutExpired:
        return 'ssh timed out after 20 s'
    if result.returncode == 0:
        return None
    error = result.stderr or ''
    if isinstance(error, bytes):
        error = error.decode(errors='replace')
    return 'ssh rc %d: %s' % (result.returncode, ' | '.join(error.strip().splitlines()[-3:]))


def visible_within(name, domain, budget):
    """Seconds until the name completes and ssh reaches it; after budget, raises with what failed last."""
    started = time.monotonic()
    failure = 'not checked'
    while time.monotonic() - started < budget:
        failure = ssh_failure(name + '.' + domain) if completes(name) else 'not in shell completion'
        if failure is None:
            return time.monotonic() - started
        time.sleep(1)
    raise RuntimeError('%s was not reachable by name within %d s: %s' % (name, budget, failure))


def rename(current, new):
    result = pb('machine', 'rename', current, new, '--json')
    if result.returncode != 0:
        raise RuntimeError('pb machine rename %s failed' % current)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    before = registration()
    if windows_target.is_windows(PB, args.target):
        TRUE[:] = ['exit', '0']
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
    finally:
        rename(renamed, args.target)
    restored = visible_within(args.target, domain, BUDGET)
    if registration() != before:
        raise RuntimeError('inventory freshness changed the enrolled identity')
    print(json.dumps({'check': 'inventory-freshness', 'target': args.target, 'status': 'PASS', 'wait_s': round(wait_seconds, 1),
                      'rename_visible_s': round(appeared, 1), 'restore_visible_s': round(restored, 1), 'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
