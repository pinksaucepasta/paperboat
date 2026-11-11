#!/usr/bin/env python3
"""Run the device-smoke checks that are ported to Windows against the Windows device, from this Linux controller.

  VICTUS_PASS=... windows_checks.py --install-json FILE --machine NAME --peer NAME [--only ID,ID] [--list]

Only pb.exe and small PowerShell snippets run on the device (winhost.py); the checks themselves run here with
PB_SMOKE_WINDOWS_PB set to the pb.exe path of the saved `pb install --json` receipt. Each check is reported as
`<id>:victus`; PASS needs exit 0 and every JSON status line the check printed to be PASS. Results go to
results/windows-<UTC>.json. Checks that need a fixture the run was not given are SKIP, never PASS.

Not ported (they need Git, systemd/launchd, sudo or a signal on the device, or a gate an external driver releases):
install_lifecycle, pairing_lifecycle's uninstall flows, ssh-other-user (the managed sshd reads one key file, no passwords),
config_sync, and the gated
preview_lifecycle/session replay and cross checks. config-sync-lifecycle and config-commands' repository rows keep Git on this
controller (pb itself needs none): it needs git, python3 and, for its SSH repository row, an sshd on this
controller's 127.0.0.1:22, TCP forwarding (ssh -R) on the device and an elevated SSH session there (it stops and
starts the config worker's service). preview-protocols and tunnel-protocols are probed from this
controller (preview_protocol_client.go, as smoke.sh does). The origins of the survival, failure and protocol checks
run here and are reached from the device through an ssh -R forward; the host service is PaperboatHostd-<instance>. ssh-tools runs scp and sftp only (Windows has no
rsync). terminal-matrix skips the rows that cut the network (it would cut this controller's SSH) and the echo
latency row. access-machine, private-address and private-https run with the device as the client only (the origin on a
Unix peer through pb exec); private_access_permission drives two Unix hosts over ssh. ssh-long holds a session for SMOKE_SSH_LONG_SECONDS (3601 for the real check; pass --timeout above it); exec-long holds one pb exec for SMOKE_EXEC_LONG_SECONDS (1801).
ENV: env-scopes, env-messages, env-delivery, env-revoke-rotation, env-ssh-hook and env-vscode-remote need the device
account's own unlocked vault (--vault-password-file: delivery reaches that account's machines); the last two drive the
device's own PowerShell over plain ssh (the hook's only Windows shell; the device must let its profile.ps1 run), and
env-vscode-remote downloads the win32-x64 VS Code server there; env-team-scopes has the device account as owner and
the second profile as member; env-undeliverable needs no vault, only a disposable target and the commands that cut it.
Narrowed on Windows: policy-token (the device creates the tokens, a Unix throwaway holds them), team-two-account (no
member-machine approval/transfer), team-business (F19-07 access and F19-10 streaming only with --f19-peer-cmd / --f19-s3-origin), completion (the PowerShell
script only). cli_credential.py is not ported; winhost.cli_secrets reads the DPAPI-protected tokens (bugreport uses it).
The ssh-setup-time … ssh-forwarding checks have the device as the TARGET (ssh_mac_rows.py --windows): CLIENT_CMD runs one shell
string on a paired Linux client, REVOKE_CLIENT_CMD on a fresh throwaway one that signs out. ssh-trust-host is ssh_trust_host.sh with ROTATE_CMD.
"""
import argparse
import contextlib
import datetime
import json
import os
from pathlib import Path
import secrets
import shlex
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request

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
    # F3-02 from the Windows device as client (winhost ssh -tt/ConPTY, no Unix pty on the device). replay is the detach/reattach half. cross
    # is the other-device half: the second client must be a second DEVICE signed in to the same account (a second attach from the same device
    # would not prove it), so it needs SMOKE_CROSS_WINDOWS_SSH (its ssh login) and SMOKE_CROSS_WINDOWS_PB (its pb.exe path); terminal_gate
    # runs the check's `resume` step there when the check asks for the other device.
    table['session-replay'] = (['session_lifecycle.py', args.peer, '--check', 'replay'], ['--peer'])
    table['session-cross'] = (['session_lifecycle.py', args.peer, '--check', 'cross'], ['--peer', 'SMOKE_CROSS_WINDOWS_SSH', 'SMOKE_CROSS_WINDOWS_PB'])
    table['session-sharing-owner'] = (['session_sharing_lifecycle.py', args.peer, '--team', team], ['--peer', '--team'])
    for name in ('lifecycle', 'tools_lifecycle', 'doctor_lifecycle', 'alias_lifecycle'):
        table['ssh-' + name.split('_')[0].replace('lifecycle', 'main')] = (['ssh_%s.py' % name, args.peer], ['--peer'])
    table['ssh-rsync'] = (['ssh_tools_lifecycle.py', args.peer, '--tools', 'rsync'], ['--peer'])
    table['local-access'] = (['local_access.py', '--controller', 'windows', '--target', args.peer], ['--peer'])
    table['cli-budget'] = (['perf_dataplane.py', '--cli-only'], [])
    table['terminal-vt'] = (['terminal_vt_input.py', args.peer], ['--peer'])
    table['ctrlc-interactive'] = (['ctrlc_interactive.py', args.peer], ['--peer'])  # a real console Ctrl-C key (ETX through ConPTY) stops pb.exe: the other interrupts are CTRL_BREAK
    table['terminal-new'] = (['terminal_lifecycle.py', args.machine, '--check', 'main', '--entry', 'new'], ['--machine'])  # C045: pb new on this device, in a directory the check chose
    for name in ('main', 'paste', 'typing'):  # paste and typing need a Unix peer (sh)
        table['terminal-' + name] = (['terminal_lifecycle.py', args.peer, '--check', name], ['--peer'])
    # about 25 min: pass --timeout 3600. N14/N12 restart the peer's host runtime (--restart-target-cmd, a shell command
    # here) and this device's daemon (pb.exe service restart over ssh, built in run_check).
    table['terminal-matrix'] = (['terminal_matrix.py', args.peer, '--restart-client-cmd', '{restart_client}'] +
                                (['--restart-target-cmd', args.restart_target_cmd] if args.restart_target_cmd else []), ['--peer'])
    for name in ('access-machine', 'private-address', 'private-https', 'path-direct', 'path-relay'):  # the device is the client; the origin runs on the peer
        table[name] = (['private_access.py', 'pair', '--target', args.peer, '--check', name], ['--peer'])
    table['exec-long'] = (['exec_long.py', args.peer, '--seconds', os.environ.get('SMOKE_EXEC_LONG_SECONDS', '')],
                           ['--peer', 'SMOKE_EXEC_LONG_SECONDS'])
    table['ssh-long'] = (['ssh_long.py', args.peer, '--seconds', os.environ.get('SMOKE_SSH_LONG_SECONDS', '')],
                         ['--peer', 'SMOKE_SSH_LONG_SECONDS'])
    table['ssh-long-interactive'] = (['ssh_long.py', args.peer, '--interactive', '--seconds', os.environ.get('SMOKE_SSH_LONG_SECONDS', '')],
                                     ['--peer', 'SMOKE_SSH_LONG_SECONDS'])  # F5-02 with a held pty shell, then exit 0
    for name, step, needs in (('ssh-setup-time', 'setup', []), ('ssh-control-outage', 'outage', ['REVOKE_CLIENT_CMD']),
                              ('ssh-refusals', 'refusals', []), ('ssh-live-revoke', 'revoke', ['REVOKE_CLIENT_CMD']),
                              ('ssh-session-audit', 'audit', []), ('ssh-offline-audit', 'offline_audit', []),
                              ('ssh-forwarding', 'forwarding', [])):
        table[name] = (['ssh_mac_rows.py', '--windows', '--alias', args.machine, '--only', step], ['--machine', 'CLIENT_CMD'] + needs)
    table['config-commands'] = (['config_commands.py'], [])
    table['config-survival'] = (['config_survival.py'], [])
    table['tunnel-manage'] = (['tunnel_manage.py'], [])
    table['tunnel-lifecycle'] = (['tunnel_lifecycle.py'], [])
    table['tunnel-public-protocols'] = (['tunnel_public_protocols.py'], [])
    table['tunnel-private-access'] = (['tunnel_private_access_windows.py', args.peer], ['--peer'])
    for mode in ('foreground', 'background'):
        table['preview-' + mode] = (['preview_lifecycle.py', '--mode', mode], [])
    table['preview-protocols'] = (['preview_protocols.py', 'victus'], [])
    table['tunnel-protocols'] = (['preview_protocols.py', 'victus', '--tunnel'], [])
    table['terminal-entry'] = (['terminal_entry.py', args.peer], ['--peer'])  # C038, C046
    table['network-drop'] = (['network_drop.py', args.peer], ['--peer'])
    table['net-blip'] = (['net_blip.py', args.peer], ['--peer'])  # F21-48: a 5 min block of pb.exe's traffic, about 7 min
    table['transfer-modes'] = (['transfer_modes_windows.py', args.peer], ['--peer', 'TARGET_CMD'])  # F22-01; TARGET_CMD: see its docstring
    table['usage-categories'] = (['usage_categories.py', args.peer], ['--peer', 'PB_SMOKE_API_CMD'])  # F13-02; see its docstring
    table['path-display'] = (['path_display.py', args.peer], ['--peer'])
    for name in ('preview_survival', 'tunnel_survival', 'tunnel_failures', 'tunnel_transfer', 'network_inventory'):
        table[name.replace('_', '-')] = ([name + '.py'], [])
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
    table['env-vault-lifecycle'] = (['env_vault_lifecycle.py', '{vault_password}', '{vault_recovery}', '--teammate'], [])
    # ENV delivery reaches the machines of the device's own account, so these use its vault (--vault-password-file, an
    # owner-only file on the device); env-vault-lifecycle and the member side of env-team-scopes use the second profile.
    vault, peer = args.vault_password_file or '', args.peer
    table['env-scopes'] = (['env_scopes.py', peer, '--password-file', vault, '--rotate'], ['--peer', '--vault_password_file'])
    # The plain ssh shells of the device itself (PowerShell with its profile); they unlock the same vault first.
    hooked = [os.environ.get('SMOKE_WINDOWS_SSH', winhost.DEFAULT_TARGET), '--password-file', vault]
    table['env-ssh-hook'] = (['env_ssh_hook.py'] + hooked, ['--vault_password_file'])
    table['env-vscode-remote'] = (['env_vscode_remote.py'] + hooked, ['--vault_password_file'])  # fetches the server
    table['env-messages'] = (['env_messages.py', peer, args.readonly_team or ''], ['--peer', '--readonly_team'])
    table['env-delivery'] = (['env_delivery.py', peer, '--expect', args.env_expect or ''], ['--peer', '--env_expect'])
    table['env-undeliverable'] = (['env_undeliverable.py', args.disposable_target or '', '--inject', args.inject or '',
                                   '--clear', args.clear or ''], ['--disposable_target', '--inject', '--clear'])
    table['env-revoke-rotation'] = (['env_revoke_rotation.py', peer, '--revoke', args.revoke_machine_id or '',
                                     '--password-file', vault], ['--peer', '--revoke_machine_id', '--vault_password_file'])
    table['env-team-scopes'] = (['env_team_scopes.py', '--team', team, '--owner-cmd', winhost.WINDOWS,
                                 '--owner-password-file', vault, '--member-cmd', winhost.WINDOWS_TEAMMATE,
                                 '--member-password-file', args.teammate_vault_password_file or '',
                                 '--member-account', '{teammate_account}', '--member-email', '{teammate_email}'],
                                ['--team', '--vault_password_file', '--teammate_vault_password_file'])
    # policy-cli is policy_cli.sh's 13 steps with pb alone. policy-token: the device creates the tokens and edits the
    # policy; the token holder is a Unix throwaway of the account (signing in with a token replaces the CLI session and
    # ends signed out, which must not happen to the device).
    table['policy-cli'] = (['policy_cli.py', peer], ['--peer'])
    table['policy-token'] = (['policy_token.py', '--allowed', args.token_allowed or '', '--other', args.token_other or '',
                              '--owner-cmd', winhost.WINDOWS, '--token-cmd', args.token_cmd or '', '--pb', args.token_pb],
                             ['--token_cmd', '--token_allowed', '--token_other'])
    # The device account owns the team and the second profile is its other member. The second profile has no machine,
    # and transfer-to-team then remove would end the device's enrollment, so approval and transfer are skipped.
    two = ['team_two_account.py', '--team', team, '--owner-ssh', winhost.WINDOWS, '--machine', '{machine_id}',
           '--member-cmd', winhost.WINDOWS_TEAMMATE]
    if args.member_cmd:  # a Unix machine of the second account: the approval and transfer flows end *its* enrollment, not the device's
        table['team-two-account'] = (two[:-1] + [args.member_cmd, '--member-machine', args.member_machine or '', '--skip', 'suspension'],
                                     ['--team', '--member_machine'])
    else:
        table['team-two-account'] = (two + ['--skip', 'approval,suspension,transfer'], ['--team'])
    table['team-suspension'] = (two + ['--skip', 'share,approval,resources,transfer', '--suspend-cmd', args.suspend_cmd or ''],
                                ['--team', '--suspend_cmd'])
    # Only pb runs on the device; the token helper, receivers and S3 client run on this controller (team_business.local_helper).
    # F19-07 access steps need a Unix peer (--f19-peer-cmd), F19-10 streaming a controller-reachable store (--f19-s3-origin).
    business = ['team_business.py', '--cmd', winhost.WINDOWS, '--api', '{server}', '--team']
    f19 = (['--alias', args.machine, '--peer-alias', args.peer, '--peer-cmd', args.f19_peer_cmd,
            '--pb', args.token_pb] if args.f19_peer_cmd else []) + (
        ['--streaming', '--s3-origin', args.f19_s3_origin, '--s3-list-cmd', args.f19_s3_list_cmd or ''] if args.f19_s3_origin else [])
    table['team-business'] = (business + [team, '--member', '{teammate_account}'] + f19, ['--team'])
    table['team-identity-commands'] = (business + [args.sso_team or '', '--identity-commands'], ['--sso_team'])
    for name in ('status_bar_lifecycle', 'completion_lifecycle', 'bugreport_lifecycle', 'inventory_freshness',
                 'session_limit', 'doctor_lifecycle'):  # inventory-freshness renames the peer and back
        table[name.replace('_lifecycle', '').replace('_', '-')] = ([name + '.py', peer], ['--peer'])
    for name in ('config_customize', 'cli_paging', 'network_inventory', 'update_observation', 'approve_refusals'):
        table[name.replace('_', '-')] = ([name + '.py'], [])
    table['machine-tags'] = (['machine_tags.py', '--owner-cmd', winhost.WINDOWS, '--target', peer], ['--peer'])
    table['free-plan'] = (['free_plan.py', args.machine], ['--machine'])  # the device account must be a Free account
    # F11 across two machines: the device and a Unix peer of the account share one repository served on --pair-repo-shell's host.
    table['config-sync-pair'] = (['config_sync_pair.py', args.pair_repo_shell or '', args.pair_repo_addr or '', winhost.WINDOWS,
                                  args.machine or '', args.pair_peer_shell or '', args.pair_peer_alias or '', '--machine-sections'],
                                 ['--machine', '--pair_repo_shell', '--pair_repo_addr', '--pair_peer_shell', '--pair_peer_alias'])
    table['nonadmin-install'] = (['nonadmin_install.py'], [])  # creates and removes a standard user on the device
    for name in ('reinstall', 'pair-refusal', 'service', 'crash-service', 'setup'):  # the daemon service stops and comes back
        table['install-' + name] = (['install_lifecycle.py', name], [])
    for fault in ('sigint', 'network'):
        table['setup-recovery-' + fault] = (['setup_recovery.py', fault], [])
    table['send-self'] = (['send_self.py', args.machine], ['--machine'])
    table['workspace-switch'] = (['workspace_lifecycle.py'], ['--empty_team'])  # PAPERBOAT_SMOKE_TEAM = --empty-team
    table['config-sync-lifecycle'] = (['config_sync_lifecycle.py', args.machine], ['--machine'])  # about 15 min
    table['config-team-default'] = (['config_team_default.py', team, args.config_repository or ''], ['--team', '--config_repository'])
    # Logs the device out and in again; the link is approved here in headless Chromium with the stored console session.
    table['auth-lifecycle'] = (['auth_lifecycle_approved.py', 'windows', peer], ['--peer', 'STATE', 'PW_CHROMIUM'])
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
    # team_two_account.py, config_team_default.py and the policy checks print one `PASS name` / `FAIL name` line a step
    statuses += [line[:4] for line in stdout.splitlines() if line.startswith(('PASS ', 'FAIL '))]
    return 'PASS' if returncode == 0 and statuses and set(statuses) == {'PASS'} else 'FAIL'


def protocol_gate(probe):
    """The external half of preview_protocols.py: probe its URL from here and release the gate it waits on.

    Returns the probe's output, which joins the check's own (its PASS line counts in the verdict)."""
    def release(line):
        try:
            phase = json.loads(line)
        except ValueError:
            return ''
        if not isinstance(phase, dict) or phase.get('state') != 'preview_protocol_required':
            return ''
        done = subprocess.run([str(probe), '--url', phase['url'], '--marker', phase['marker']], capture_output=True,
                              text=True, timeout=170, stdin=subprocess.DEVNULL)
        Path(phase['gate' if done.returncode == 0 else 'failure_gate']).touch(mode=0o600)
        return done.stdout
    return release


def browser_gate(line):
    """The independent browser of preview_lifecycle.py: fetch its URL from here, release the gate only if the page
    holds the marker. Returns '' (nothing to add to the check's output)."""
    try:
        state = json.loads(line)
    except ValueError:
        return ''
    if not isinstance(state, dict) or state.get('state') != 'browser_required':
        return ''
    ok, deadline = False, time.monotonic() + 120
    while time.monotonic() < deadline and not ok:  # the public name takes a moment to resolve
        try:
            with urllib.request.urlopen(state['url'], timeout=20) as reply:
                ok = reply.status == 200 and state['marker'] in reply.read().decode(errors='replace')
        except OSError:
            time.sleep(3)
    Path(state['gate' if ok else 'failure_gate']).touch(mode=0o600)
    return ''


def terminal_gate(environment):
    """The second client of session_lifecycle.py --check cross: run its --check resume on the other device, then release its gate."""
    other = dict(environment, SMOKE_WINDOWS_SSH=os.environ['SMOKE_CROSS_WINDOWS_SSH'], PB_SMOKE_WINDOWS_PB=os.environ['SMOKE_CROSS_WINDOWS_PB'])

    def release(line):
        try:
            phase = json.loads(line)
        except ValueError:
            return ''
        if not isinstance(phase, dict) or phase.get('state') != 'terminal_attach_required':
            return ''
        command = [sys.executable, str(HERE / 'session_lifecycle.py'), phase['target'], '--check', 'resume',
                   '--session-id', phase['session_id'], '--expected-state', phase['expected_state'],
                   '--expected-account', phase['expected_account']]
        done = subprocess.run(command, cwd=HERE, capture_output=True, text=True, timeout=300, env=other, stdin=subprocess.DEVNULL)
        ok = done.returncode == 0 and '"status": "PASS"' in done.stdout
        Path(phase['gate' if ok else 'failure_gate']).touch(mode=0o600)
        return done.stdout + done.stderr
    return release


def run_streaming(command, environment, timeout, on_state=None):
    """subprocess.run with captured output, echoing each `state` line (an approval link, a gate) to stderr as it comes.

    on_state(line) may act on such a line (release a gate); what it returns is added to the output."""
    with tempfile.TemporaryFile('w+') as errors, subprocess.Popen(
            command, cwd=HERE, env=environment, stdout=subprocess.PIPE, stderr=errors, text=True,
            stdin=subprocess.DEVNULL) as process:
        killer = threading.Timer(timeout, process.kill)
        killer.start()
        try:
            lines = []
            for line in process.stdout:
                lines.append(line)
                if line.startswith('{"state"') or '"state": "approval_required"' in line or '"state": "terminal_attach_required"' in line:
                    print(line.rstrip(), file=sys.stderr, flush=True)
                    if on_state:
                        lines.append(on_state(line))
            process.wait()
        finally:
            killer.cancel()
        errors.seek(0)
        return subprocess.CompletedProcess(command, process.returncode, ''.join(lines), errors.read())


def run_check(check, argv, args, device):
    environment = dict(os.environ, PB_SMOKE_WINDOWS_PB=device.pb_exe)
    if check == 'workspace-switch':
        environment['PAPERBOAT_SMOKE_TEAM'] = args.empty_team
    replacements = {}
    with contextlib.ExitStack() as stack:
        if check == 'env-vault-lifecycle':  # a fresh vault on a fresh account; its secret files stay on the device
            vault = stack.enter_context(device.temp_dir('pb-vault'))
            for name in ('password', 'recovery'):  # pb reads secrets only from owner-only files
                (vault / name).write_text(secrets.token_urlsafe(24))
                (vault / name).chmod(0o600)
            replacements = {'{vault_password}': str(vault / 'password'), '{vault_recovery}': str(vault / 'recovery')}
        if '{token}' in argv:  # the check may delete its copy of the token; never the original
            scratch = Path(stack.enter_context(tempfile.TemporaryDirectory(prefix='pb-windows-checks-')))
            shutil.copyfile(os.environ['FINAL_FIXTURES_TOKEN_FILE'], scratch / 'fixtures-token')
            (scratch / 'fixtures-token').chmod(0o600)
            replacements.update({'{token}': str(scratch / 'fixtures-token'), '{output}': str(scratch / 'rows.json'),
                                 '{run}': 'win' + secrets.token_hex(4)})
        if '{teammate_account}' in argv:  # the second signed-in profile of the device
            account = json.loads(winhost.side_run(winhost.WINDOWS_TEAMMATE, ['auth', 'status', '--json'], timeout=60).stdout)['account']
            replacements.update({'{teammate_account}': account['id'], '{teammate_email}': account['email']})
        if '{machine_id}' in argv:
            replacements['{machine_id}'] = json.loads((device.runtime_dir() / 'machine-registration.json').read_text())['machine_id']
        if '{restart_client}' in argv:
            command = "powershell -NoProfile -NonInteractive -Command \"& '%s' service restart\"" % device.pb_exe
            replacements['{restart_client}'] = shlex.join(device.ssh_prefix(device.target, command))
            environment = device.ssh_environment() or environment  # the password (never printed) for sshpass -e
            environment['PB_SMOKE_WINDOWS_PB'] = device.pb_exe
        if '{server}' in argv:
            replacements.update({
                '{server}': json.loads((device.runtime_dir() / 'machine-registration.json').read_text())['server_url'],
                '{version}': device.run([device.pb_exe, '--version']).stdout.strip().splitlines()[0]})
        on_state = None
        if argv[0] == 'preview_protocols.py':  # built before the check starts: its gate waits only 180 s
            probe = Path(stack.enter_context(tempfile.TemporaryDirectory(prefix='pb-protocol-probe-'))) / 'client'
            subprocess.run(['go', 'build', '-o', str(probe), str(HERE / 'preview_protocol_client.go')], cwd=HERE.parent.parent,
                           check=True, capture_output=True, timeout=900, env=dict(os.environ, GOMAXPROCS='2', GOFLAGS='-p=2'))
            on_state = protocol_gate(probe)
        if argv[0] == 'preview_lifecycle.py':
            on_state = browser_gate
        if argv[0] == 'session_lifecycle.py' and 'cross' in argv:
            on_state = terminal_gate(environment)
        command = [sys.executable, str(HERE / argv[0])] + [replacements.get(word, word) for word in argv[1:]]
        started = time.monotonic()
        done = run_streaming(command, environment, args.timeout, on_state)
    # The whole output is kept beside the result file (owner-only): per-row receipts and failure causes live there.
    output = HERE / 'results' / 'windows-output' / ('%s-%s.out' % (time.strftime('%Y%m%dT%H%M%SZ', time.gmtime()), check))
    output.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    with os.fdopen(os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), 'w') as handle:
        handle.write(done.stdout + '\n--- stderr ---\n' + done.stderr)
    outcome = verdict(done.returncode, done.stdout)
    detail = ((done.stdout.strip().splitlines() or [''])[-1][:300] if outcome == 'PASS'
              else 'exit %s: %s' % (done.returncode, (done.stderr.strip() or done.stdout.strip())[-300:]))
    return outcome, round(time.monotonic() - started), detail


def build_parser():
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
    parser.add_argument('--restart-target-cmd', default='', help="terminal-matrix N14: shell command restarting the peer's host runtime")
    parser.add_argument('--vault-password-file', help="owner-only file on the device: the device account's ENV vault password")
    parser.add_argument('--teammate-vault-password-file', help="the same for the device's second account profile")
    parser.add_argument('--readonly-team', help='env-messages: a team the device account is a read-only member of')
    parser.add_argument('--env-expect', help='env-delivery: the delivery state the caller made (empty, applied, pending, undeliverable)')
    parser.add_argument('--revoke-machine-id', help='env-revoke-rotation: a throwaway machine of the device account, revoked')
    parser.add_argument('--disposable-target', help='env-undeliverable: a machine of the account that is safe to disturb')
    parser.add_argument('--inject', help="env-undeliverable: controller command that cuts the target's control plane")
    parser.add_argument('--clear', help='env-undeliverable: controller command that undoes --inject')
    parser.add_argument('--token-cmd', help='policy-token: ssh/docker prefix of a Unix throwaway machine of the device account; '
                        'its CLI session is replaced by the token')
    parser.add_argument('--token-allowed', help='policy-token: the machine name the token may exec on')
    parser.add_argument('--token-other', help='policy-token: another machine name of the account')
    parser.add_argument('--token-pb', default='/home/pbt/.local/bin/pb',
                        help='pb on the Unix API-token peer for policy-token and team-business')
    parser.add_argument('--member-cmd', help='team-two-account: ssh/docker prefix of a machine of the second account (enables approval and transfer)')
    parser.add_argument('--member-machine', help="team-two-account: that machine's id")
    parser.add_argument('--suspend-cmd', help='team-suspension: controller command that runs SQL from stdin on the server database')
    parser.add_argument('--sso-team', help='team-identity-commands: a team the device account owns, with SSO and a directory group')
    parser.add_argument('--empty-team', help='workspace-switch: a team of the device account without machines')
    parser.add_argument('--config-repository', help='config-team-default: a GitHub config repository connected to the account')
    parser.add_argument('--pair-repo-shell', help='config-sync-pair: shell prefix of the host that serves the repository')
    parser.add_argument('--pair-repo-addr', help='config-sync-pair: that host as the controller and the peer reach it')
    parser.add_argument('--pair-peer-shell', help='config-sync-pair: shell prefix of the Unix peer')
    parser.add_argument('--pair-peer-alias', help="config-sync-pair: the peer's machine name")
    parser.add_argument('--f19-peer-cmd', help='team-business: shell prefix of a Unix throwaway machine of the account (--peer is its name); enables the F19-07 access steps')
    parser.add_argument('--f19-s3-origin', help='team-business: host:port of an S3-compatible store reachable from this controller; enables F19-10 streaming')
    parser.add_argument('--f19-s3-list-cmd', help='team-business: controller command listing that store')
    parser.add_argument('--only', default='', help='comma list of check ids')
    parser.add_argument('--timeout', type=int, default=1800)
    parser.add_argument('--list', action='store_true')
    return parser


def main():
    parser = build_parser()
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
            except (subprocess.SubprocessError, winhost.DeviceError, OSError, ValueError, KeyError) as error:
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
