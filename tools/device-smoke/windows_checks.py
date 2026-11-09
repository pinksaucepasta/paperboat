#!/usr/bin/env python3
"""Run the device-smoke checks that are ported to Windows against the Windows device, from this Linux controller.

  VICTUS_PASS=... windows_checks.py --install-json FILE --machine NAME --peer NAME [--only ID,ID] [--list]

Only pb.exe and small PowerShell snippets run on the device (winhost.py); the checks themselves run here with
PB_SMOKE_WINDOWS_PB set to the pb.exe path of the saved `pb install --json` receipt. Each check is reported as
`<id>:victus`; PASS needs exit 0 and every JSON status line the check printed to be PASS. Results go to
results/windows-<UTC>.json. Checks that need a fixture the run was not given are SKIP, never PASS.

Not ported (they need Git, systemd/launchd, sudo or a signal on the device, or a gate an external driver releases):
terminal_matrix, install_lifecycle, pairing_lifecycle's uninstall flows, ssh_*, preview_survival/tunnel_survival,
config_sync/config_sync_lifecycle and config_commands' repository rows, the gated preview_lifecycle/preview_protocols/
session replay and cross checks, and the target side of exec_lifecycle/transfer_* when the target is Windows.
"""
import argparse
import contextlib
import datetime
import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import sys
import tempfile
import time

import install_path
import winhost

HERE = Path(__file__).resolve().parent
SECRET_FIXTURES = ('FINAL_FIXTURES_URL', 'FINAL_FIXTURES_TOKEN_FILE', 'SMOKE_DNS_ZONE')


def checks(args):
    """id -> (script argv, fixtures it needs). The argv is what `python3` runs here."""
    team = args.team
    domain = ['--fixtures-url', os.environ.get('FINAL_FIXTURES_URL', ''), '--zone', os.environ.get('SMOKE_DNS_ZONE', '')]
    table = {
        'account-inventory': (['account_inventory.py'], []),
        'command-rows': (['command_rows.py', args.machine, '--peer', args.peer], ['--machine', '--peer']),
        'local-access-settings': (['local_access_settings.py'], []),
        'team-commands': (['team_commands.py', '--team', team], ['--team']),
        'team-machines': (['team_machines.py', team], ['--team']),
        'team-lifecycle': (['team_lifecycle.py', '--team', team], ['--team']),
        'team-roles': (['team_lifecycle.py', '--roles', '--team', team], ['--team']),
        'inbox-manage': (['inbox_manage.py', args.peer], ['--peer']),
        'tunnel-domain': (['tunnel_domain.py', '--fixtures-token-file', '{token}'] + domain, list(SECRET_FIXTURES)),
    }
    table['tunnel-command-rows'] = (['tunnel_command_rows.py', '--run', '{run}', '--build', 'windows', '--version', '{version}',
                                     '--server', '{server}', '--output', '{output}', '--fixtures-token-file', '{token}']
                                    + domain, list(SECRET_FIXTURES))
    table['exec-main'] = (['exec_lifecycle.py', args.peer, '--check', 'exec'], ['--peer'])
    table['readiness-main'] = (['exec_lifecycle.py', args.peer, '--check', 'readiness'], ['--peer'])
    table['exec-repeat'] = (['exec_lifecycle.py', args.peer, '--check', 'repeated'], ['--peer'])
    table['exec-cancel'] = (['exec_cancel.py', args.peer], ['--peer'])
    table['exec-messages'] = (['exec_messages.py'], [])
    table['transfer-manage'] = (['transfer_manage.py', args.peer], ['--peer'])
    table['transfer-multi'] = (['transfer_lifecycle.py', args.peer], ['--peer'])
    table['transfer-resume'] = (['transfer_resume.py', args.peer], ['--peer'])
    table['transfer-errors'] = (['transfer_errors.py', args.peer], ['--peer'])
    table['session-manage'] = (['session_lifecycle.py', args.peer, '--check', 'manage'], ['--peer'])
    table['session-sharing-owner'] = (['session_sharing_lifecycle.py', args.peer, '--team', team], ['--peer', '--team'])
    table['config-commands'] = (['config_commands.py'], [])
    table['config-survival'] = (['config_survival.py'], [])
    table['tunnel-manage'] = (['tunnel_manage.py'], [])
    table['tunnel-lifecycle'] = (['tunnel_lifecycle.py'], [])
    table['tunnel-public-protocols'] = (['tunnel_public_protocols.py'], [])
    for name in ('manage', 'inspect', 'access'):
        table['preview-' + name] = (['preview_%s.py' % name, 'victus'], [])
    for name in ('rename', 'settings', 'availability', 'capabilities'):
        table['machine-' + name] = (['machine_lifecycle.py', name], [])
    table['env-vault-lifecycle'] = (['env_vault_lifecycle.py', '{vault_password}', '{vault_recovery}'], [])
    return table


def missing(needs, args):
    gaps = []
    for need in needs:
        if need.startswith('--'):
            if not getattr(args, need[2:]):
                gaps.append(need)
        elif not os.environ.get(need):
            gaps.append(need)
    return gaps


def verdict(returncode, stdout):
    """PASS only for exit 0 with at least one JSON status line and no status other than PASS."""
    statuses = []
    for line in stdout.splitlines():
        try:
            record = json.loads(line)
        except ValueError:
            continue
        if isinstance(record, dict) and 'status' in record:
            statuses.append(record['status'])
    return 'PASS' if returncode == 0 and statuses and set(statuses) == {'PASS'} else 'FAIL'


def run_check(check, argv, args, device):
    environment = dict(os.environ, PB_SMOKE_WINDOWS_PB=device.pb_exe)
    replacements = {}
    with contextlib.ExitStack() as stack:
        if check == 'env-vault-lifecycle':  # a fresh vault on a fresh account; its secret files stay on the device
            vault = stack.enter_context(device.temp_dir('pb-vault'))
            (vault / 'password').write_text(secrets.token_urlsafe(24))
            (vault / 'recovery').write_text(secrets.token_urlsafe(24))
            replacements = {'{vault_password}': str(vault / 'password'), '{vault_recovery}': str(vault / 'recovery')}
        if '{token}' in argv:  # the check may delete its copy of the token; never the original
            scratch = Path(stack.enter_context(tempfile.TemporaryDirectory(prefix='pb-windows-checks-')))
            shutil.copyfile(os.environ['FINAL_FIXTURES_TOKEN_FILE'], scratch / 'fixtures-token')
            (scratch / 'fixtures-token').chmod(0o600)
            replacements.update({'{token}': str(scratch / 'fixtures-token'), '{output}': str(scratch / 'rows.json'),
                                 '{run}': 'win' + secrets.token_hex(4)})
        if '{server}' in argv:
            replacements.update({
                '{server}': json.loads((device.runtime_dir() / 'machine-registration.json').read_text())['server_url'],
                '{version}': device.run([device.pb_exe, '--version']).stdout.strip().splitlines()[0]})
        command = [sys.executable, str(HERE / argv[0])] + [replacements.get(word, word) for word in argv[1:]]
        started = time.monotonic()
        done = subprocess.run(command, cwd=HERE, env=environment, capture_output=True, text=True, timeout=args.timeout,
                              stdin=subprocess.DEVNULL)
    outcome = verdict(done.returncode, done.stdout)
    detail = ((done.stdout.strip().splitlines() or [''])[-1][:300] if outcome == 'PASS'
              else 'exit %s: %s' % (done.returncode, (done.stderr.strip() or done.stdout.strip())[-300:]))
    return outcome, round(time.monotonic() - started), detail


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--install-json', type=Path, help='saved `pb install --json` output of the Windows device')
    parser.add_argument('--machine', help="the device's own machine name (command-rows)")
    parser.add_argument('--peer', help='another machine of the account (command-rows, inbox-manage)')
    parser.add_argument('--team', default=os.environ.get('PAPERBOAT_SMOKE_TEAM', ''), help='the retained paid team')
    parser.add_argument('--only', default='', help='comma list of check ids')
    parser.add_argument('--timeout', type=int, default=1800)
    parser.add_argument('--list', action='store_true')
    args = parser.parse_args()
    table = checks(args)
    if args.list:
        print('\n'.join(sorted(table)))
        return 0
    if not args.install_json:
        parser.error('--install-json is required')
    only = set(filter(None, args.only.split(',')))
    if only - set(table):
        parser.error('unknown check: ' + ', '.join(sorted(only - set(table))))
    pb_exe = install_path.windows_executable(json.loads(args.install_json.read_text()))
    os.environ['PB_SMOKE_WINDOWS_PB'] = pb_exe
    device = winhost.device()
    results = []
    for check, (argv, needs) in sorted(table.items()):
        if only and check not in only:
            continue
        gaps = missing(needs, args)
        if gaps:
            outcome = ('SKIP', 0, 'needs ' + ', '.join(gaps))
        else:
            try:
                outcome = run_check(check, argv, args, device)
            except (subprocess.TimeoutExpired, winhost.DeviceError, OSError) as error:
                outcome = ('FAIL', 0, str(error)[:300])
        results.append({'id': check + ':victus', 'status': outcome[0], 'seconds': outcome[1], 'detail': outcome[2]})
        print('%-34s %-5s %5ss  %s' % (check + ':victus', outcome[0], outcome[1], outcome[2][:100]), flush=True)
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
    path = HERE / 'results' / ('windows-%s.json' % stamp)
    path.parent.mkdir(exist_ok=True)
    path.write_text(json.dumps({'at': stamp, 'results': results}))
    print('results:', path)
    return 1 if any(row['status'] == 'FAIL' for row in results) else 0


if __name__ == '__main__':
    sys.exit(main())
