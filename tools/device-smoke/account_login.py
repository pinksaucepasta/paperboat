#!/usr/bin/env python3
"""Exercise normal CLI login/logout; a browser must approve each emitted link."""
import argparse
import json
import os
import selectors
import subprocess
import tempfile
import time

import winhost
from install_lifecycle import registration
from pairing_lifecycle import invoke


def approval_receipt(process):
    deadline, record = time.monotonic() + 45, b''
    with selectors.DefaultSelector() as selector:
        selector.register(process.stdout, selectors.EVENT_READ)
        while b'\n' not in record:
            remaining = deadline - time.monotonic()
            if remaining <= 0 or not selector.select(remaining):
                raise RuntimeError('login did not produce an approval within45 seconds')
            chunk = os.read(process.stdout.fileno(), 4096)
            if not chunk or len(record) + len(chunk) > 65536:
                raise RuntimeError('login approval response is absent or oversized')
            record += chunk
    receipt = json.loads(record.split(b'\n', 1)[0])
    return receipt.get('data', receipt)


def announce(approval):
    if approval.get('state') != 'approval_required':
        raise RuntimeError('login did not request browser approval')
    print(json.dumps({'check': 'browser-login', 'state': 'approval_required',
                      'approval_url': approval['approval_url']}), flush=True)


def local_login(pb, arguments):
    with subprocess.Popen([str(pb), *arguments],
                          stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                          stderr=subprocess.PIPE) as process:
        try:
            announce(approval_receipt(process))
            process.communicate(timeout=600)
            if process.returncode:
                raise RuntimeError('browser login failed; output withheld')
        except Exception as error:
            if process.poll() is None:
                process.kill()
            output, errors = process.communicate(timeout=10)
            with tempfile.NamedTemporaryFile(mode='w', prefix='paperboat-login-error-',
                                             delete=False) as diagnostic:
                json.dump({'stdout': output.decode(errors='replace'),
                           'stderr': errors.decode(errors='replace')}, diagnostic)
            raise RuntimeError(str(error) + '; private diagnostics: ' + diagnostic.name) from None


def remote_login(pb, arguments):
    """The same flow with pb.exe running on the Windows device and its output read from files there."""
    with pb.device.temp_dir('pb-login') as directory:
        process = pb.device.spawn([str(pb), *arguments], directory)
        try:
            deadline = time.monotonic() + 45
            while b'\n' not in process.stdout():
                if time.monotonic() > deadline or process.returncode() is not None:
                    raise RuntimeError('login did not produce an approval within 45 seconds')
                time.sleep(1)
            receipt = json.loads(process.stdout().split(b'\n', 1)[0])
            announce(receipt.get('data', receipt))
            if process.wait(600):
                raise RuntimeError('browser login failed; output withheld')
        except Exception as error:
            with tempfile.NamedTemporaryFile(mode='w', prefix='paperboat-login-error-',
                                             delete=False) as diagnostic:
                json.dump({'stdout': process.stdout().decode(errors='replace'),
                           'stderr': process.stderr().decode(errors='replace')}, diagnostic)
            raise RuntimeError(str(error) + '; private diagnostics: ' + diagnostic.name) from None
        finally:
            process.kill()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--auth-alias', action='store_true')
    parser.add_argument('--reauth', action='store_true')
    args = parser.parse_args()
    pb = winhost.pb_path()
    enrolled = registration()
    current = invoke(pb, 'auth', 'status', '--json')
    if not current['signed_in'] or current['account']['id'] != enrolled['account_id']:
        raise RuntimeError('sign-in test requires the enrolled machine account')
    prefix = ['auth'] if args.auth_alias else []
    if not args.reauth:
        logout = invoke(pb, *prefix, 'logout', '--json')
        if logout['signed_out'] is not True or logout['server_revocation'] != 'complete':
            raise RuntimeError('logout did not confirm local and server revocation')
        if invoke(pb, 'auth', 'status', '--json')['signed_in'] is not False:
            raise RuntimeError('auth status remained signed in after logout')
    login = [*prefix, 'login', '--no-browser', '--json']
    if args.reauth:
        login.append('--reauth')
    (remote_login if winhost.device() else local_login)(pb, login)
    active = invoke(pb, 'auth', 'status', '--json')
    if not active['signed_in'] or active['account']['id'] != enrolled['account_id']:
        raise RuntimeError('approved login did not restore the enrolled account')
    if registration() != enrolled:
        raise RuntimeError('login/logout changed the machine enrollment')
    invoke(pb, 'machine', 'list', '--json')
    if invoke(pb, 'doctor', '--json')['overall'] != 'healthy':
        raise RuntimeError('machine did not remain healthy after login/logout')
    result = {'check': 'browser-login', 'status': 'PASS',
              'command': 'auth login' if args.auth_alias else 'login',
              'same_enrolled_account': True, 'enrollment_preserved': True,
              'mode': 'reauth' if args.reauth else 'logout-login', 'signed_in': True}
    if not args.reauth:
        result['server_revocation'] = 'complete'
    print(json.dumps(result), flush=True)


if __name__ == '__main__':
    main()
