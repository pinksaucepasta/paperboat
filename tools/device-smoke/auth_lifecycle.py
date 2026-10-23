#!/usr/bin/env python3
"""C022-C024: logout revokes the CLI session, signed-out commands say `pb login`, login restores access.

Browser approval is human/browser work: the approval link is written to URL_FILE and this script
waits (up to 8 minutes) for it to be approved elsewhere, then verifies the signed-in state.
"""
import argparse
import json
from pathlib import Path
import subprocess
import time

PB = Path.home() / '.local/bin/pb'
SIGNED_OUT_COMMANDS = [('machine', 'list'), ('env', 'list'), ('exec', 'TARGET', '--', 'true'),
                       ('session', 'list', 'TARGET'), ('team', 'list')]
# Local-only commands keep working signed out (the Inbox path is this computer's setting).
SIGNED_OUT_LOCAL = [('inbox', 'path')]


def run(*args, timeout=60):
    result = subprocess.run([str(PB), *args], capture_output=True, text=True, timeout=timeout,
                            stdin=subprocess.DEVNULL)
    return result.returncode, result.stdout + result.stderr


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('url_file', type=Path)
    args = parser.parse_args()
    code, out = run('auth', 'status', '--json')
    if json.loads(out).get('signed_in') is not True:
        raise RuntimeError('must start signed in')
    code, out = run('logout', '--json')
    receipt = json.loads(out)
    if code or not receipt['data']['signed_out'] or receipt['data']['server_revocation'] != 'complete':
        raise RuntimeError('logout did not revoke the session')
    status = run('auth', 'status')[1]
    if 'Not signed in' not in status:
        raise RuntimeError('auth status does not say Not signed in')
    unclear = []
    for command in SIGNED_OUT_COMMANDS:
        argv = [args.target if part == 'TARGET' else part for part in command]
        code, out = run(*argv)
        if code == 0 or 'pb login' not in out or len(out) > 700:
            unclear.append(' '.join(command[:2]))
    for command in SIGNED_OUT_LOCAL:
        if run(*command)[0] != 0:
            unclear.append(' '.join(command) + ' (local, must work signed out)')
    login = subprocess.Popen([str(PB), 'login', '--no-browser', '--json'], stdout=subprocess.PIPE,
                             stderr=subprocess.STDOUT, text=True, stdin=subprocess.DEVNULL)
    try:
        link = json.loads(login.stdout.readline())['data']
        if link.get('state') != 'approval_required' or not link['approval_url'].startswith('https://'):
            raise RuntimeError('login did not produce an approval link')
        args.url_file.write_text(link['approval_url'] + '\n')
        deadline = time.monotonic() + 480
        while login.poll() is None and time.monotonic() < deadline:
            time.sleep(2)
        if login.poll() is None:
            raise RuntimeError('login was not approved in time')
        if login.returncode != 0:
            raise RuntimeError('login exited %s: %s' % (login.returncode, login.stdout.read()[-300:]))
    finally:
        if login.poll() is None:
            login.kill()
        login.wait()
    restored = json.loads(run('auth', 'status', '--json')[1])
    if restored.get('signed_in') is not True:
        raise RuntimeError('login completed but status is signed out')
    deadline = time.monotonic() + 120
    while time.monotonic() < deadline and run('exec', args.target, '--', 'true')[0] != 0:
        time.sleep(4)
    if run('exec', args.target, '--', 'true')[0] != 0:
        raise RuntimeError('exec did not work after login')
    if unclear:
        raise RuntimeError('signed-out commands without a pb login hint: ' + ', '.join(unclear))
    print(json.dumps({'check': 'auth-lifecycle', 'target': args.target, 'status': 'PASS',
                      'logout_revokes': True, 'signed_out_hint': True, 'login_restores': True}), flush=True)


if __name__ == '__main__':
    main()
