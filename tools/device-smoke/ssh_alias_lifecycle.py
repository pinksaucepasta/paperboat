#!/usr/bin/env python3
"""Use plain OpenSSH through the server-configured, generated machine alias."""
import argparse
import getpass
import json
import re
import shlex
import urllib.request
import uuid

from exec_lifecycle import run
from install_lifecycle import registration
from pairing_lifecycle import runtime_root
import windows_target
import winhost


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    before = registration()
    binding = json.loads((runtime_root() / 'machine-registration.json').read_text())
    with urllib.request.urlopen(binding['server_url'].rstrip('/') + '/v1/client-configuration', timeout=15) as response:
        configuration = json.load(response)['data']
    domain = configuration['local_access_base_domain']
    if not domain or '/' in domain or any(c.isspace() for c in domain):
        raise RuntimeError('server discovery returned an invalid local access domain')
    binary = winhost.which('ssh')
    if binary is None:
        raise RuntimeError('OpenSSH is not installed')
    host = args.target + '.' + domain
    # Plain OpenSSH keeps its normal local username. Cross-device accounts may
    # differ; use the target account explicitly, as a user or editor would.
    pb = winhost.pb_path()
    windows = windows_target.is_windows(pb, args.target)  # no id or printf there: PowerShell through cmd.exe
    username, errors = run(pb, 'ssh', args.target, '--',
                           windows_target.ssh_command(windows_target.username_script()) if windows else 'id -un')
    username = username.decode().strip()
    if errors or not re.fullmatch(r'[A-Za-z0-9_.-]+', username):
        raise RuntimeError('managed SSH did not identify one exact target login')
    local = winhost.device().env('USERNAME') if winhost.device() else getpass.getuser()
    destination = host if username == local else username + '@' + host
    marker = 'PB_PLAIN_SSH_' + uuid.uuid4().hex
    output, errors = run(binary, '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=15', destination,
                         windows_target.ssh_command(windows_target.print_script(marker)) if windows
                         else 'printf %s ' + shlex.quote(marker))
    if output != marker.encode() or errors:
        raise RuntimeError('plain generated-alias SSH changed the exact command result')
    if registration() != before:
        raise RuntimeError('plain SSH changed the enrolled identity')
    print(json.dumps({'check': 'ssh-alias', 'target': args.target, 'status': 'PASS',
                      'server_domain': True, 'plain_openssh': True,
                      'explicit_target_user': destination != host, 'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
