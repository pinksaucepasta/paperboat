#!/usr/bin/env python3
"""Verify a real Unix administrator refusal without creating an OS account."""
import argparse
import json
import os
from pathlib import Path
import pwd
import shutil
import subprocess
import tempfile


def check(user, binary):
    account = pwd.getpwnam(user)
    if os.geteuid() != 0 or account.pw_uid == 0:
        raise RuntimeError('root must select an existing non-root test account')
    platform = os.uname().sysname
    base = ('/usr/local/libexec/paperboat' if platform == 'Linux' else
            '/Library/PrivilegedHelperTools/Paperboat')
    slot = Path(base) / 'users' / ('u' + str(account.pw_uid))
    if slot.exists():
        raise RuntimeError('test account already has an installation; refusing to touch it')
    prefix = ['sudo', '-u', user, '--']
    permission = subprocess.run(prefix + ['sudo', '-n', 'true'],
                                stdin=subprocess.DEVNULL, capture_output=True, timeout=10)
    if permission.returncode == 0:
        raise RuntimeError('selected account has administrator rights')
    with tempfile.TemporaryDirectory(prefix='pb-refusal-') as directory:
        root = Path(directory)
        executable = root / 'pb'
        shutil.copyfile(binary, executable)
        executable.chmod(0o700)
        for path in (root, executable):
            os.chown(path, account.pw_uid, account.pw_gid)
        result = subprocess.run(prefix + ['env', 'HOME=' + directory,
                                'XDG_STATE_HOME=' + str(root / 'state'),
                                'XDG_RUNTIME_DIR=' + directory,
                                str(executable), 'install', '--json',
                                '--install-dir', str(root / 'bin')],
                                stdin=subprocess.DEVNULL, capture_output=True,
                                text=True, timeout=45)
        try:
            receipt = json.loads(result.stdout)
        except ValueError:
            raise RuntimeError('non-admin install did not return a JSON refusal') from None
        message = receipt.get('error', {}).get('message', '')
        if result.returncode == 0 or receipt.get('ok') is True or not all(
                phrase in message for phrase in ('supplied Paperboat executable',
                                                  'administrator access', 'pb install')):
            raise RuntimeError('non-admin refusal lost its installation step or recovery action')
        if slot.exists() or (root / 'bin/pb').exists():
            raise RuntimeError('refused installation created a system slot or command')
    print(json.dumps({'check': 'nonadmin-install', 'status': 'PASS',
                      'specific_refusal': True, 'no_installation': True}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--user', required=True)
    parser.add_argument('--binary', required=True)
    args = parser.parse_args()
    check(args.user, args.binary)


if __name__ == '__main__':
    main()
