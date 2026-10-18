#!/usr/bin/env python3
"""F10-03 on one machine: empty ENV runs normally; a read-only Team write is denied clearly.

The signed-in account must be a read-only member of TEAM with its ENV vault unlocked.
The denial must say nothing changed, and the vault must stay usable afterwards.
"""
import argparse
import json
from pathlib import Path
import subprocess
import uuid

PB = Path.home() / '.local/bin/pb'
DENIED = 'do not have write access'


def run(*args, stdin=None, timeout=60):
    result = subprocess.run([str(PB), *args], input=stdin, capture_output=True, text=True, timeout=timeout)
    return result.returncode, result.stdout, result.stderr


def data(result, step):
    code, out, _ = result
    try:
        receipt = json.loads(out)
    except ValueError:
        raise RuntimeError(step + ': no JSON receipt') from None
    if code or receipt.get('ok') is False:
        raise RuntimeError(step + ': failed; code=' + str(receipt.get('error', {}).get('code', 'unknown')))
    return receipt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('team')
    args = parser.parse_args()
    name = 'PB_SMOKE_' + uuid.uuid4().hex[:8].upper()
    # Empty ENV: commands run normally.
    names = data(run('env', 'list', '--json'), 'env list')['data']['names']
    if names:
        raise RuntimeError('personal ENV is not empty; the empty-ENV case needs an empty scope')
    code, out, _ = run('exec', args.target, '--', 'true')
    if code:
        raise RuntimeError('exec with no ENV assigned failed')
    # Read-only Team write: definitive, plain denial.
    code, out, err = run('env', 'set', name, '--team', args.team, '--workspace', args.team,
                         '--value-stdin', stdin='denied-value')
    message = out + err
    if code == 0:
        raise RuntimeError('read-only Team write succeeded')
    if DENIED not in message or 'nothing was changed' not in message:
        raise RuntimeError('denied write did not say the member lacks write access and nothing changed')
    if 'denied-value' in message:
        raise RuntimeError('denied write printed the submitted value')
    # Nothing staged: the vault accepts a personal write and delivers it.
    data(run('env', 'set', name, '--value-stdin', '--json', stdin='ok-value'), 'personal set')
    try:
        code, out, _ = run('exec', args.target, '--', 'sh', '-c', 'echo "[$' + name + ']"')
        if code or '[ok-value]' not in out:
            raise RuntimeError('personal value was not delivered after the denied Team write')
    finally:
        challenge = json.loads(run('env', 'unset', name, '--json')[1])
        token = challenge.get('data', {}).get('confirmation_token')
        if token:
            run('env', 'unset', name, '--json', '--confirm', token)
    if run('env', 'list', '--json')[1].count(name):
        raise RuntimeError('cleanup left the smoke variable behind')
    print(json.dumps({'check': 'env-messages', 'target': args.target, 'status': 'PASS',
                      'empty_env_runs': True, 'denied_write_message': True,
                      'vault_usable_after_denial': True}), flush=True)


if __name__ == '__main__':
    main()
