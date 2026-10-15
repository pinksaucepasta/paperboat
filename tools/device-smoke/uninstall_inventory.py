#!/usr/bin/env python3
"""Read-only Unix product inventory, run as root after an uninstall process exits."""
import argparse
import glob
import json
import os
from pathlib import Path
import pwd
import ssl
import subprocess


def run(args):
    return subprocess.run(args, capture_output=True, text=True, timeout=15,
                          stdin=subprocess.DEVNULL)


def retained(path, inbox):
    if path.is_symlink():
        return True
    if not path.exists() or path == inbox or inbox in path.parents:
        return False
    if path not in inbox.parents:
        return True
    return any(retained(child, inbox) for child in path.iterdir())


def inventory(account, inbox):
    home = Path(account.pw_dir)
    linux = os.uname().sysname == 'Linux'
    uid = account.pw_uid
    categories = {}

    def add(category, count):
        if count:
            categories[category] = categories.get(category, 0) + count

    if linux:
        paths = ['/usr/local/libexec/paperboat', '/var/lib/paperboat',
                 '/var/lib/paperboat-installer', '/var/lib/paperboat-machineguard',
                 '/usr/local/libexec/paperboat-machineguard',
                 '/usr/local/libexec/paperboat-deviceguard']
        patterns = ['/etc/systemd/system/*paperboat*',
                    '/etc/systemd/system/*/*paperboat*', '/run/paperboat*',
                    '/var/lib/paperboat-*', '/var/cache/paperboat*',
                    '/usr/local/share/ca-certificates/paperboat*',
                    '/etc/ssl/certs/paperboat*',
                    '/usr/local/share/man/man1/pb*.1']
        users = ['.config/paperboat', '.local/state/paperboat',
                 '.local/share/paperboat', '.cache/paperboat',
                 '.config/systemd/user/paperboatd.service',
                 '.config/systemd/user/default.target.wants/paperboatd.service']
        for scope in ([], ['--user']):
            prefix = (['sudo', '-u', account.pw_name, '--', 'env',
                       'XDG_RUNTIME_DIR=/run/user/' + str(uid)] if scope else [])
            result = run(prefix + ['systemctl', *scope, 'list-unit-files',
                         '--no-pager', '--no-legend', 'paperboat*'])
            if result.returncode:
                raise RuntimeError('could not inspect installed service declarations')
            add('services', len(result.stdout.splitlines()))
        certificates = ssl.create_default_context().get_ca_certs()
        add('ca_trust', sum(any(value.startswith('Paperboat Local Root CA')
                               for group in cert.get('subject', ())
                               for key, value in group if key == 'commonName')
                            for cert in certificates))
        result = run(['nft', 'list', 'tables'])
        if result.returncode:
            raise RuntimeError('could not inspect nftables')
        add('firewall', sum('paperboat' in line.lower()
                            for line in result.stdout.splitlines()))
        bus = Path('/run/user') / str(uid) / 'bus'
        if bus.exists():
            prefix = ['sudo', '-u', account.pw_name, '--', 'busctl',
                      '--address=unix:path=' + str(bus)]
            owner = run(prefix + ['call', 'org.freedesktop.DBus',
                        '/org/freedesktop/DBus', 'org.freedesktop.DBus',
                        'NameHasOwner', 's', 'org.freedesktop.secrets'])
            if owner.returncode:
                raise RuntimeError('could not inspect credential service ownership')
            if owner.stdout.strip() == 'b true':
                result = run(prefix + ['call', 'org.freedesktop.secrets',
                             '/org/freedesktop/secrets', 'org.freedesktop.Secret.Service',
                             'SearchItems', 'a{ss}', '1', 'service', 'paperboat'])
                if result.returncode:
                    raise RuntimeError('could not inspect credential item metadata')
                add('credentials', result.stdout.count('/org/freedesktop/secrets/collection/'))
    else:
        paths = ['/Library/PrivilegedHelperTools/Paperboat',
                 '/Library/Application Support/Paperboat',
                 '/Library/PrivilegedHelperTools/io.paperboat.machineguard',
                 '/Library/PrivilegedHelperTools/io.paperboat.deviceguard']
        patterns = ['/Library/LaunchDaemons/*paperboat*',
                    '/private/var/log/com.pinksaucepasta.paperboat*',
                    '/var/run/paperboat*']
        users = ['Library/Application Support/Paperboat',
                 'Library/Caches/paperboat',
                 'Library/LaunchAgents/com.pinksaucepasta.paperboatd.plist']
        for prefix in ([], ['launchctl', 'asuser', str(uid)]):
            result = run(prefix + ['launchctl', 'list'])
            if result.returncode:
                raise RuntimeError('could not inspect registered launch jobs')
            add('services', sum('paperboat' in line.lower()
                                for line in result.stdout.splitlines()))
        result = run(['security', 'find-certificate', '-a', '-p', '-c',
                      'Paperboat Local Root CA', '/Library/Keychains/System.keychain'])
        if result.returncode not in (0, 44):
            raise RuntimeError('could not inspect system CA trust')
        add('ca_trust', result.stdout.count('-----BEGIN CERTIFICATE-----'))
        result = run(['dscl', '.', '-search', '/Users', 'RealName',
                      'Paperboat protected listener'])
        if result.returncode:
            raise RuntimeError('could not inspect guard accounts')
        add('accounts', len(result.stdout.splitlines()))
        result = run(['/sbin/pfctl', '-a', 'com.apple', '-s', 'Anchors'])
        if result.returncode:
            raise RuntimeError('could not inspect PF anchors')
        add('firewall', sum('paperboat' in line.lower()
                            for line in result.stdout.splitlines()))
        result = run(['sudo', '-u', account.pw_name, '--', 'security',
                      'find-generic-password', '-s', 'paperboat',
                      str(home / 'Library/Keychains/login.keychain-db')])
        if result.returncode not in (0, 44):
            raise RuntimeError('could not inspect credential item metadata')
        add('credentials', int(result.returncode == 0))

    paths.extend(str(home / value) for value in users)
    paths.extend(str(home / value) for value in ['.local/bin/pb',
                 '.ssh/paperboat_config', '.ssh/paperboat-managed-ssh.pub',
                 '.ssh/.paperboat-config-install-v1.json',
                 '.ssh/.paperboat-config-transaction-v1.json'])
    for pattern in patterns:
        paths.extend(glob.glob(pattern))
    add('files', sum(retained(Path(path), inbox) for path in set(paths)))
    ssh_config = home / '.ssh/config'
    if ssh_config.exists():
        add('ssh_configuration', ssh_config.read_bytes().count(
            b'paperboat-managed-ssh-include-v1'))
    processes = run(['ps', '-axo', 'comm'])
    if processes.returncode:
        raise RuntimeError('could not inspect running processes')
    add('processes', sum('paperboat' in value.lower() or Path(value.strip()).name == 'pb'
                         for value in processes.stdout.splitlines()[1:]))
    return categories


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--user', required=True)
    parser.add_argument('--inbox', required=True)
    args = parser.parse_args()
    if os.geteuid() != 0:
        raise RuntimeError('inventory requires root for complete OS metadata')
    inbox = Path(args.inbox)
    if not inbox.is_absolute() or inbox.is_symlink():
        raise RuntimeError('inbox must be an absolute non-symlink directory')
    leftovers = inventory(pwd.getpwnam(args.user), inbox)
    print(json.dumps({'check': 'uninstall-inventory',
                      'status': 'FAIL' if leftovers else 'PASS',
                      'leftovers': leftovers, 'total': sum(leftovers.values())}))
    return bool(leftovers)


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except Exception as error:
        print(json.dumps({'check': 'uninstall-inventory', 'status': 'FAIL',
                          'reason': str(error)}))
        raise SystemExit(1)
