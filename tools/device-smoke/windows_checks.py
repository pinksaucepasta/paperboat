#!/usr/bin/env python3
"""Run the device-smoke checks that are ported to Windows against the Windows device, from this Linux controller.

  VICTUS_PASS=... windows_checks.py --install-json FILE --machine NAME --peer NAME [--only ID,ID] [--list]

Only pb.exe and small PowerShell snippets run on the device (winhost.py); the checks themselves run here with
PB_SMOKE_WINDOWS_PB set to the pb.exe path of the saved `pb install --json` receipt. Each check is reported as
`<id>:victus`; PASS needs exit 0 and every JSON status line the check printed to be PASS. Results go to
results/windows-<UTC>.json. Checks that need a fixture the run was not given are SKIP, never PASS.

Not ported (they need Git, systemd/launchd, sudo or a signal on the device, or a gate an external driver releases):
install_lifecycle, pairing_lifecycle's uninstall flows, the ssh_*.sh and ssh_mac_* checks,
preview_survival/tunnel_survival, config_sync/config_sync_lifecycle and config_commands' repository rows, and the gated
preview_lifecycle/preview_protocols/session replay and cross checks. ssh-tools runs scp and sftp only (Windows has no
rsync). terminal-matrix skips the rows that cut the network (it would cut this controller's SSH) and the echo
latency row. access-machine, private-address and private-https run with the device as the client only (the origin on a
Unix peer through pb exec); private_access_permission drives two Unix hosts over ssh. ssh-long holds a session for SMOKE_SSH_LONG_SECONDS (3601 for the real check; pass --timeout above it).
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
import threading
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
    for name in ('lifecycle', 'tools_lifecycle', 'doctor_lifecycle', 'alias_lifecycle'):
        table['ssh-' + name.split('_')[0].replace('lifecycle', 'main')] = (['ssh_%s.py' % name, args.peer], ['--peer'])
    for name in ('main', 'paste', 'typing'):  # paste and typing need a Unix peer (sh)
        table['terminal-' + name] = (['terminal_lifecycle.py', args.peer, '--check', name], ['--peer'])
    table['terminal-matrix'] = (['terminal_matrix.py', args.peer], ['--peer'])  # about 25 min: pass --timeout 3600
    for name in ('access-machine', 'private-address', 'private-https'):  # the device is the client; the origin runs on the peer
        table[name] = (['private_access.py', 'pair', '--target', args.peer, '--check', name], ['--peer'])
    table['ssh-long'] = (['ssh_long.py', args.peer, '--seconds', os.environ.get('SMOKE_SSH_LONG_SECONDS', '')],
                         ['--peer', 'SMOKE_SSH_LONG_SECONDS'])
    table['config-commands'] = (['config_commands.py'], [])
    table['config-survival'] = (['config_survival.py'], [])
    table['tunnel-manage'] = (['tunnel_manage.py'], [])
    table['tunnel-lifecycle'] = (['tunnel_lifecycle.py'], [])
    table['tunnel-public-protocols'] = (['tunnel_public_protocols.py'], [])
    for name in ('manage', 'inspect', 'access'):
        table['preview-' + name] = (['preview_%s.py' % name, 'victus'], [])
    for name in ('rename', 'settings', 'availability', 'capabilities'):
        table['machine-' + name] = (['machine_lifecycle.py', name], [])
    # Interactive: its approval link is echoed to stderr while it waits for a browser approval.
    table['account-login'] = (['account_login.py'], [])
    # The teammate's sends are driven from outside: its gate line (state inbox_sender_required) is echoed to stderr.
    table['inbox-approval'] = (['inbox_approval.py', '--team', team, '--source-machine-id', args.teammate_machine_id or '',
                                '--source-account-id', args.teammate_account_id or ''],
                               ['--team', '--teammate_machine_id', '--teammate_account_id'])
    # The Windows device is the team member; the owner's machine is reached through --owner-cmd.
    table['policy-team'] = (['policy_team.py', '--team', team, '--machine-id', args.owner_machine_id or '',
                             '--machine-name', args.owner_machine_name or '', '--owner-cmd', args.owner_cmd or '',
                             '--owner-pb', args.owner_pb, '--member-cmd', 'windows'],
                            ['--team', '--owner_cmd', '--owner_machine_id', '--owner_machine_name'])
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
    statuses += [line.split()[1] for line in stdout.splitlines() if line.startswith('RESULT ')]  # policy_team.py
    return 'PASS' if returncode == 0 and statuses and set(statuses) == {'PASS'} else 'FAIL'


def run_streaming(command, environment, timeout):
    """subprocess.run with captured output, echoing each `state` line (an approval link, a gate) to stderr as it comes."""
    with tempfile.TemporaryFile('w+') as errors, subprocess.Popen(
            command, cwd=HERE, env=environment, stdout=subprocess.PIPE, stderr=errors, text=True,
            stdin=subprocess.DEVNULL) as process:
        killer = threading.Timer(timeout, process.kill)
        killer.start()
        try:
            lines = []
            for line in process.stdout:
                lines.append(line)
                if line.startswith('{"state"') or '"state": "approval_required"' in line:
                    print(line.rstrip(), file=sys.stderr, flush=True)
            process.wait()
        finally:
            killer.cancel()
        errors.seek(0)
        return subprocess.CompletedProcess(command, process.returncode, ''.join(lines), errors.read())


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
        done = run_streaming(command, environment, args.timeout)
    # The whole output is kept beside the result file (owner-only): per-row receipts and failure causes live there.
    output = HERE / 'results' / 'windows-output' / ('%s-%s.out' % (time.strftime('%Y%m%dT%H%M%SZ', time.gmtime()), check))
    output.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    with os.fdopen(os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), 'w') as handle:
        handle.write(done.stdout + '\n--- stderr ---\n' + done.stderr)
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
    parser.add_argument('--teammate-machine-id', help="inbox-approval: the teammate's sending machine")
    parser.add_argument('--teammate-account-id', help="inbox-approval: the teammate's account")
    parser.add_argument('--owner-cmd', help="policy-team: ssh/docker prefix that runs pb as the team owner")
    parser.add_argument('--owner-machine-id', help='policy-team: the owner machine shared into the team')
    parser.add_argument('--owner-machine-name')
    parser.add_argument('--owner-pb', default='.local/bin/pb')
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
