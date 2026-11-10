#!/usr/bin/env python3
"""Read-only Unix product inventory, run as root after an uninstall process exits."""
import argparse
import glob
import json
from itertools import islice
import os
from pathlib import Path
import pwd
import ssl
import subprocess


CONTAINER_CGROUPS = ('docker', 'libpod', 'containerd', 'kubepods', 'lxc', 'crio')


def in_host_scope(pid, linux=None):
    """Linux: the process is in this host's own PID namespace and outside container cgroups. A docker host lists
    every container's processes; another user's container is not this account's product."""
    if linux is None:
        linux = os.uname().sysname == 'Linux'
    if not linux:
        return True
    try:
        if os.readlink(f'/proc/{pid}/ns/pid') != os.readlink('/proc/self/ns/pid'):
            return False
        with open(f'/proc/{pid}/cgroup') as cgroup:
            return not any(word in cgroup.read() for word in CONTAINER_CGROUPS)
    except OSError:
        return False  # gone, or not readable: not provably this host's


def product_process_count(listing, in_scope=in_host_scope):
    """Count `pid comm` lines of paperboat processes in scope."""
    count = 0
    for line in listing:
        pid, _, name = line.strip().partition(' ')
        name = name.strip()
        if pid.isdigit() and name and ('paperboat' in name.lower() or Path(name).name == 'pb') and in_scope(int(pid)):
            count += 1
    return count


def run(args):
    return subprocess.run(args, capture_output=True, text=True, timeout=15,
                          stdin=subprocess.DEVNULL)


LEFTOVER_FILES = []


def retained(path, inbox):
    if path.is_symlink():
        return True
    if not path.exists() or path == inbox or inbox in path.parents:
        return False
    if path not in inbox.parents:
        return True
    return any(retained(child, inbox) for child in path.iterdir())


def retained_leaf(path, inbox):
    # The deepest retained entry, so a kept parent directory names what keeps it (bounded by bounded_entries).
    while path.is_dir() and not path.is_symlink():
        child = next((c for c in sorted(bounded_entries(path)) if retained(c, inbox)), None)
        if child is None:
            break
        path = child
    return str(path)


def bounded_entries(directory):
    entries = list(islice(directory.iterdir(), 4097))
    if len(entries) > 4096:
        raise RuntimeError('system metadata inventory exceeds its entry bound')
    return entries


def owned_manual_pages(sections):
    marker = b'.\\" Paperboat managed manual v1\n'
    count = 0
    for section in sections:
        if not section.exists():
            continue
        for entry in bounded_entries(section):
            if entry.name != 'pb.1' and not (entry.name.startswith('pb-') and entry.name.endswith('.1')):
                continue
            if entry.is_symlink() or not entry.is_file():
                continue
            with entry.open('rb') as file:
                count += file.read(len(marker)) == marker
    return count


def owned_ssh_configuration(path):
    if not path.exists():
        return 0
    count = 0
    legacy = (b'# BEGIN paperboat-managed-ssh-v1', b'# END paperboat-managed-ssh-v1')
    for line in configuration_bytes(path).splitlines():
        stripped = line.strip()
        if stripped in legacy:
            count += 1
            continue
        directive, separator, comment = stripped.rpartition(b'#')
        fields = directive.split(None, 1)
        count += bool(separator and fields and fields[0].lower() == b'include' and
                      comment.strip() == b'paperboat-managed-ssh-include-v1')
    return count


def configuration_bytes(path):
    with path.open('rb') as file:
        data = file.read((1 << 20) + 1)
    if len(data) > 1 << 20:
        raise RuntimeError('configuration file exceeds the inventory size bound')
    return data

def systemd_service_count(command):
    # A filtered list exits1 when no units match; enumerate successfully, then count.
    result = run([*command, 'list-unit-files', '--no-pager', '--no-legend'])
    if result.returncode:
        raise RuntimeError('could not inspect installed service declarations')
    return sum(line.split()[0].startswith('paperboat')
               for line in result.stdout.splitlines() if line.strip())


def linux_os_inventory(account, home, add):
    uid = account.pw_uid
    paths = ['/usr/local/libexec/paperboat', '/var/lib/paperboat',
             '/var/lib/paperboat-installer', '/var/lib/paperboat-machineguard',
             '/usr/local/libexec/paperboat-machineguard']
    patterns = ['/etc/systemd/system/*paperboat*',
                '/etc/systemd/system/*/*paperboat*', '/run/paperboat*',
                '/var/lib/paperboat-*', '/var/cache/paperboat*',
                '/usr/local/share/ca-certificates/paperboat*',
                '/etc/ssl/certs/paperboat*']
    users = ['.config/paperboat', '.local/state/paperboat',
             '.local/share/paperboat', '.cache/paperboat',
             '.config/systemd/user/paperboatd.service',
             '.config/systemd/user/default.target.wants/paperboatd.service']
    for scope in ([], ['--user']):
        prefix = (['sudo', '-u', account.pw_name, '--', 'env',
                   'XDG_RUNTIME_DIR=/run/user/' + str(uid)] if scope else [])
        add('services', systemd_service_count(prefix + ['systemctl', *scope]))
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
    return paths, patterns, users


def darwin_pf_rule_count():
    result = run(['/sbin/pfctl', '-a', 'com.apple', '-s', 'Anchors'])
    if result.returncode:
        raise RuntimeError('could not inspect PF anchors')
    count = 0
    # PF retains empty anchor names; uninstall must remove their filtering/NAT rules.
    for name in result.stdout.splitlines():
        name = name.strip()
        if 'paperboat' not in name.lower():
            continue
        anchor = name if name.startswith('com.apple/') else 'com.apple/' + name
        for kind in ('rules', 'nat'):
            rules = run(['/sbin/pfctl', '-a', anchor, '-s', kind])
            if rules.returncode:
                raise RuntimeError('could not inspect owned PF rules')
            count += len(rules.stdout.splitlines())
    return count


def darwin_os_inventory(account, home, add):
    uid = account.pw_uid
    paths = ['/Library/PrivilegedHelperTools/Paperboat',
             '/Library/Application Support/Paperboat',
             '/Library/PrivilegedHelperTools/io.paperboat.machineguard']
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
    add('firewall', darwin_pf_rule_count())
    result = run(['sudo', '-u', account.pw_name, '--', 'security',
                  'find-generic-password', '-s', 'paperboat',
                  str(home / 'Library/Keychains/login.keychain-db')])
    if result.returncode not in (0, 44):
        raise RuntimeError('could not inspect credential item metadata')
    add('credentials', int(result.returncode == 0))
    return paths, patterns, users


def inventory(account, inbox):
    home = Path(account.pw_dir)
    linux = os.uname().sysname == 'Linux'
    categories = {}

    def add(category, count):
        if count:
            categories[category] = categories.get(category, 0) + count

    inspect = linux_os_inventory if linux else darwin_os_inventory
    paths, patterns, users = inspect(account, home, add)
    sections = [Path('/usr/local/share/man/man1')]
    if linux:
        sections.append(home / '.local/share/man/man1')
    add('manual_pages', owned_manual_pages(sections))

    paths.extend(str(home / value) for value in users)
    paths.extend(str(home / value) for value in ['.local/bin/pb',
                 '.ssh/paperboat_config', '.ssh/paperboat-managed-ssh.pub',
                 '.ssh/.paperboat-config-install-v1.json',
                 '.ssh/.paperboat-config-transaction-v1.json'])
    for pattern in patterns:
        paths.extend(glob.glob(pattern))
    kept = sorted(retained_leaf(Path(path), inbox) for path in set(paths) if retained(Path(path), inbox))
    LEFTOVER_FILES[:] = kept[:20]  # names only (never contents), so a failure says which file stayed
    add('files', len(kept))
    add('ssh_configuration', owned_ssh_configuration(home / '.ssh/config'))
    processes = run(['ps', '-axo', 'pid=,comm='])
    if processes.returncode:
        raise RuntimeError('could not inspect running processes')
    add('processes', product_process_count(processes.stdout.splitlines()))
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
                      'leftovers': leftovers, 'total': sum(leftovers.values()),
                      'file_paths': LEFTOVER_FILES}))
    return bool(leftovers)


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except Exception as error:
        print(json.dumps({'check': 'uninstall-inventory', 'status': 'FAIL',
                          'reason': str(error)}))
        raise SystemExit(1)
