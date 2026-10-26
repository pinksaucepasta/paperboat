#!/usr/bin/env python3
"""Pair two disposable machines, run MCP acceptance, revoke their exact IDs."""
import argparse
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
import tempfile
import uuid

from pairing_lifecycle import command_diagnostic


HERE = Path(__file__).resolve().parent
REGISTRATION = '.local/state/paperboat/runtime/machine-registration.json'


def remote(host, arguments, *, script=None, timeout=180):
    with tempfile.TemporaryFile() as output, tempfile.TemporaryFile() as errors:
        result = subprocess.run(['ssh', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=10', host,
                                 shlex.join(arguments)], input=script, text=True,
                                stdout=output, stderr=errors, timeout=timeout)
        if output.tell() > 2 << 20 or errors.tell() > 1 << 20:
            raise RuntimeError('remote fixture output exceeded its bound')
        output.seek(0)
        stdout = output.read()
        if result.returncode:
            errors.seek(0)
            path = command_diagnostic(['ssh', host, shlex.join(arguments)], stdout, errors.read())
            raise RemoteFailure(path)
        return stdout.decode()


def registration(host, container=None):
    code = ("import json; from pathlib import Path; d=json.loads((Path.home()/" +
            repr(REGISTRATION) + ").read_text()); "
            "print(json.dumps({k:d[k] for k in ('machine_id','account_id','inbox_path')}))")
    command = ['python3', '-c', code]
    if container:
        command = ['sudo', '-n', 'docker', 'exec', '-u', 'pbt', '-e', 'HOME=/home/pbt',
                   container, *command]
    return json.loads(remote(host, command, timeout=20))


def cleanup(args, run_id, containers, accounts):
    # Only this unpredictable run prefix is discovered. Every mutation uses --id.
    identities = {}
    failures = []
    for container in containers:
        try:
            accounts.add(registration(args.host, container)['account_id'])
        except Exception:
            pass  # Pairing may have failed before writing registration; discover below.
    try:
        remote(args.host, ['sudo', '-n', 'docker', 'rm', '-f', *containers], timeout=45)
    except Exception:
        failures.append('container removal failed')
    base = [sys.executable, str(HERE / 'cleanup_test_account.py'), '--host', args.cleanup_host,
            '--stale', '0 seconds']
    for account in accounts:
        found = subprocess.run([*base, '--account', account, '--prefix', run_id + '-'],
                               capture_output=True, text=True, timeout=130)
        if found.returncode:
            failures.append('exact fixture discovery failed')
        for identity in re.findall(r'machine_[0-9a-f-]{36}', found.stdout):
            identities[identity] = account
    revoked = []
    for identity, account in identities.items():
        result = subprocess.run([*base, '--account', account, '--id', identity, '--apply'],
                                capture_output=True, text=True, timeout=130)
        if result.returncode or identity not in result.stdout:
            failures.append('exact machine revocation failed: ' + identity)
        else:
            revoked.append(identity)
    paths = ['/tmp/' + run_id, *['/tmp/' + c + '-pbt-token' for c in containers]]
    try:
        remote(args.host, ['rm', '-rf', '--', *paths], timeout=20)
    except Exception:
        failures.append('fixture file removal failed')
    return {'revoked_ids': revoked, 'failures': failures}


def setup(args, run_id, containers):
    root = '/tmp/' + run_id
    remote(args.host, ['install', '-d', '-m', '700', root])
    for source, destination in [(args.binary, root + '/pb'), (args.probe, root + '/mcp-smoke')]:
        subprocess.run(['scp', '-q', source, args.host + ':' + destination], check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=90)
    remote(args.host, ['chmod', '700', root + '/pb', root + '/mcp-smoke'])
    script = 'set -euo pipefail\nFRESH_BINARY=' + shlex.quote(root + '/pb') + '\n'
    script += (HERE / 'fresh_pair_lib.sh').read_text() + '\n'
    for container in containers:
        script += 'start_ct ' + shlex.quote(container) + '\n'
        script += ('sudo -n docker exec ' + shlex.quote(container) +
                   ' test ! -e /home/pbt/' + REGISTRATION + '\n')
        script += 'pair_user ' + shlex.quote(container) + ' pbt 1000 ' + shlex.quote(container) + '\n'
    remote(args.host, ['bash', '-s'], script=script, timeout=360)


def run_checks(args, run_id, target, source):
    target_state = registration(args.host, target)
    source_state = registration(args.host, source)
    if target_state['machine_id'] == source_state['machine_id']:
        raise RuntimeError('source and target share an enrolled identity')
    if target_state['account_id'] != source_state['account_id']:
        raise RuntimeError('source and target were paired to different accounts')
    command = ['sudo', '-n', 'docker', 'exec', '-u', 'pbt', '-e', 'HOME=/home/pbt', '-e',
               'XDG_RUNTIME_DIR=/run/user/1000', source, '/home/pbt/.local/bin/pb',
               '--workspace', 'personal', 'wait', target_state['machine_id'], '--for', 'transport',
               '--timeout', '60s', '--json']
    remote(args.host, command, timeout=75)
    command = ['/tmp/' + run_id + '/mcp-smoke', '--source-container', source,
               '--target-container', target, '--source-id', source_state['machine_id'],
               '--target-id', target_state['machine_id'], '--target-inbox', target_state['inbox_path'],
               '--run-id', run_id]
    # Keep per-row PASS/FAIL output even when the script exits nonzero.
    wrapped = shlex.join(command) + '; status=$?; printf "MCP_EXIT=%s\\n" "$status"'
    output = remote(args.host, ['bash', '-c', wrapped], timeout=420)
    rows = [json.loads(line) for line in output.splitlines() if line.startswith('{')]
    if not any(line == 'MCP_EXIT=0' for line in output.splitlines()):
        raise CheckFailure(rows)
    return rows


class CheckFailure(RuntimeError):
    def __init__(self, rows):
        self.rows = rows
        super().__init__('MCP device acceptance failed')


class RemoteFailure(RuntimeError):
    def __init__(self, path):
        self.diagnostic_path = str(path)
        super().__init__('remote fixture step failed')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ['host', 'binary', 'probe', 'build-commit', 'server-version', 'cleanup-host', 'output']:
        parser.add_argument('--' + name, required=True)
    args = parser.parse_args()
    if not re.fullmatch(r'[0-9a-f]{40}', args.build_commit):
        parser.error('--build-commit must be the complete source commit')
    run_id = 'lanesdk' + uuid.uuid4().hex[:16]
    target, source = run_id + '-t', run_id + '-c'
    result = {'check': 'fresh-pair-mcp', 'build_commit': args.build_commit,
              'server_version': args.server_version, 'run_id': run_id, 'rows': []}
    accounts = {registration(args.host)['account_id']}
    try:
        setup(args, run_id, [target, source])
        result['rows'] = run_checks(args, run_id, target, source)
    except CheckFailure as error:
        result['rows'] = error.rows
        result['failure'] = str(error)
    except Exception as error:
        result['failure'] = 'fixture failed: ' + type(error).__name__
        if getattr(error, 'diagnostic_path', None):
            result['diagnostic_path'] = error.diagnostic_path
    finally:
        result['cleanup'] = cleanup(args, run_id, [target, source], accounts)
    if len(result['rows']) == 4 and len(result['cleanup']['revoked_ids']) != 2:
        result['cleanup']['failures'].append('both fresh machine IDs were not revoked')
    result['status'] = ('PASS' if not result.get('failure') and
                        not result['cleanup']['failures'] and len(result['rows']) == 4 else 'FAIL')
    output = Path(args.output)
    with os.fdopen(os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), 'w') as handle:
        json.dump(result, handle, indent=2)
    print(json.dumps(result))
    return int(result['status'] != 'PASS')


if __name__ == '__main__':
    raise SystemExit(main())
