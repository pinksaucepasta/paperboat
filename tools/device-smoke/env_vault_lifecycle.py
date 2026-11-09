#!/usr/bin/env python3
"""F10-02 on headless Linux: lock, unlock, password, recovery, recover, remove, resume and reset.

Usage: env_vault_lifecycle.py PASSWORD_FILE RECOVERY_FILE
The signed-in account's vault uses that password (a vault is created with it when the
account has none) and the recovery code (owner-only files). The vault ends unlocked with the same password and a
fresh recovery code written over RECOVERY_FILE. No ENV value is touched.
"""
import argparse
import json
import os
from pathlib import Path
import secrets
import tempfile

from pairing_lifecycle import command_result
import winhost

PB = winhost.pb_path()


def device_path(value):
    """A secret file named on the command line lives on the device under test."""
    return winhost.RemotePath(winhost.device(), value) if winhost.device() else Path(value)


def temp_dir():
    device = winhost.device()
    return device.temp_dir('pb-vault') if device else tempfile.TemporaryDirectory(prefix='paperboat-vault-')


def write_secret(path, value):
    if isinstance(path, Path):
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, 'w') as handle:
            handle.write(value)
    else:
        path.write_text(value)


def replace(source, target):
    if isinstance(source, Path):
        os.replace(source, target)
    else:
        target.write_bytes(source.read_bytes())


def run(*args, stdin=None):
    result = command_result(PB, 'env', 'vault', *args, '--json', input=stdin, timeout=90)
    try:
        receipt = json.loads(result.stdout)
    except ValueError:
        receipt = {}
    return result.returncode == 0 and receipt.get('ok') is True, receipt


def need(ok_receipt, step):
    ok, receipt = ok_receipt
    if not ok:
        raise RuntimeError(step + ' failed; code=' + str(receipt.get('error', {}).get('code', 'unknown')))
    return receipt.get('data', {})


def refuse(ok_receipt, step):
    if ok_receipt[0]:
        raise RuntimeError(step + ' succeeded but must be refused')


def owner_only(path):
    # On Windows the profile's inherited ACL keeps the file private; only Unix modes can be checked here.
    if (isinstance(path, Path) and path.stat().st_mode & 0o077) or not path.read_text().strip():
        raise RuntimeError('secret file is not owner-only and non-empty')


def personal_inventory():
    return json.loads(command_result(PB, 'env', 'list', '--json', timeout=60).stdout)['data']


def account_id():
    return json.loads(command_result(PB, 'auth', 'status', '--json', timeout=30).stdout)['account']['id']


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('password_file', type=device_path)
    parser.add_argument('recovery_file', type=device_path)
    args = parser.parse_args()
    original = args.password_file
    owner_only(original)
    owner_only(args.recovery_file)
    with temp_dir() as directory:
        base = directory if winhost.device() else Path(directory)
        replacement = base / 'replacement'
        write_secret(replacement, 'pbsmoke-' + secrets.token_urlsafe(24))
        unlock = lambda path: run('unlock', '--password-file', str(path))
        first = unlock(original)
        if not first[0] and first[1].get('error', {}).get('code') == 'not_found':  # an account without a vault yet
            need(run('init', '--password-file', str(original), '--recovery-file', str(base / 'init-recovery')), 'init')
            first = unlock(original)
        need(first, 'unlock')
        need(run('lock'), 'lock')
        refuse(run('password', '--password-stdin', stdin='x'), 'password change while locked')
        need(unlock(original), 'unlock after lock')
        # Password rewrap: the new password opens it, the old one no longer does.
        need(run('password', '--password-stdin', stdin=replacement.read_text()), 'password change')
        need(run('lock'), 'lock after password change')
        refuse(unlock(original), 'unlock with the replaced password')
        need(unlock(replacement), 'unlock with the new password')
        need(run('password', '--password-stdin', stdin=original.read_text()), 'password restore')
        need(run('lock'), 'lock after restore')
        need(unlock(original), 'unlock with the restored password')
        # Recovery: replace the code, then recover with it; the newest valid code
        # always ends up in RECOVERY_FILE, even when a step fails.
        codes = [base / 'new-recovery', base / 'final-recovery']
        try:
            need(run('recovery', '--recovery-file', str(codes[0])), 'recovery replace')
            owner_only(codes[0])
            need(run('lock'), 'lock before recover')
            need(run('recover', '--recovery-input-file', str(codes[0]), '--password-stdin',
                     '--recovery-file', str(codes[1]), stdin=original.read_text()), 'recover')
            owner_only(codes[1])
            need(run('lock'), 'lock after recover')
            need(unlock(original), 'unlock after recover')
            # Local custody removal is not a reset: the password restores it.
            need(run('remove', '--confirm', 'REMOVE LOCAL ENV ' + account_id()), 'remove local custody')
            refuse(run('password', '--password-stdin', stdin='x'), 'key operation after custody removal')
            need(unlock(original), 'unlock after custody removal')
            need(run('resume'), 'resume')
            # Reset replaces the personal keys and deletes every personal value.
            mark = 'PB_RESET_MARK'
            marked = command_result(PB, 'env', 'set', mark, '--value-stdin', '--json', input='reset-me', timeout=60)
            if marked.returncode:
                raise RuntimeError('could not set the value that reset must delete')
            before = personal_inventory()
            if mark not in before['names']:
                raise RuntimeError('the value reset must delete is not listed')
            codes.append(base / 'reset-recovery')
            need(run('reset', '--confirm', 'RESET ENV ' + account_id(), '--password-file', str(original),
                     '--recovery-file', str(codes[-1])), 'reset')
            owner_only(codes[-1])
            after = personal_inventory()
            if after['names'] or after['key_epoch'] <= before['key_epoch']:
                raise RuntimeError('reset did not delete personal values and rotate the key epoch')
            need(run('lock'), 'lock after reset')
            need(unlock(original), 'unlock after reset')
        finally:
            for newest in reversed(codes):
                if newest.exists():
                    replace(newest, args.recovery_file)
                    break
    print(json.dumps({'check': 'env-vault-lifecycle', 'status': 'PASS',
                      'lock_unlock': True, 'password_rewrap': True, 'recovery_replace_recover': True,
                      'remove_then_unlock': True, 'resume': True, 'reset_deletes_values': True}), flush=True)


if __name__ == '__main__':
    main()
