#!/usr/bin/env python3
"""F16-01..03 / C243 / C264 / C265: ENV reaches plain ssh shells through `pb env hook`.

  env_ssh_hook.py SSH_HOST

Runs from a controller over plain ssh. The host's pb must be logged in with an unlocked ENV vault
for the active workspace. Sets a random personal probe value, installs the bash hook (twice: one
line), then checks that a non-interactive `ssh host cmd` and an interactive ssh shell see the
value (compared by SHA-256, never printed), that a cached hook run is fast, that a missing runtime
or missing login loads nothing within the budget, and that uninstall removes only its line.
C264: with the variable unset in an interactive shell, `pb env load` (the hook's pb function) loads it there.
C265: `pb env run -- sh -c ...` gives the child the value with the variable unset in the caller, and passes
the child's exit code through.
C266: `pb env names` lists the probe's name (no value in its output) and `--json` names its layer (personal);
`pb env names access off` refuses the listing (`env run` still works), `on` lists again.
Cleans up the probe and the hook on every exit.

  env_ssh_hook.py SSH_HOST [--password-file FILE_ON_THE_DEVICE]     with PB_SMOKE_WINDOWS_PB set (winhost.py)

A Windows device has one hook, PowerShell's (cmd.exe has none: `pb env run -- <command>` is its way), and Windows
OpenSSH hands a remote command to cmd.exe. So the plain ssh command is `ssh HOST powershell ...` with the profile
loaded (never -NoProfile) and the interactive shell is `ssh -tt HOST powershell`; no execution policy is passed, as
a user would not. The hook line must be once in each of the two profile.ps1 files (Windows PowerShell and
PowerShell 7) and uninstall must restore both. pb's own commands (env set, hook --install, env run, env names) run
through winhost, whose PowerShell is -NoProfile, so the caller of `env run` never has the variable.
--password-file unlocks the device account's ENV vault first.

A macOS host is detected over ssh (`uname -s`): its login shell is zsh, so the hook goes in ~/.zshenv
(zsh reads it for `ssh host cmd` and interactive shells), hashes use shasum and timing uses perl, because
macOS date has no %N. Over ssh the host's login keychain must already be unlocked for the ENV vault.
"""
import argparse
import base64
import contextlib
import hashlib
import json
import re
import secrets
import subprocess
import sys
import time

import windows_target
import winhost

PB = '$HOME/.local/bin/pb'
PROBE = 'PB_ENV_HOOK_PROBE'
# `pb env unset` previews first and needs the preview's confirmation code.
UNSET = (f'c=$({PB} env unset {PROBE} --json 2>/dev/null | python3 -c "import json,sys;'
         f'print(json.load(sys.stdin).get(\'data\',{{}}).get(\'confirmation_token\',\'\'))");'
         f' [ -n "$c" ] && {PB} env unset {PROBE} --json --confirm "$c" >/dev/null 2>&1')

# macOS has no python3 guarantee on a non-interactive ssh PATH: pull the token out with sed.
TOKEN_SED = r"""sed -n 's/.*"confirmation_token" *: *"\([^"]*\)".*/\1/p'"""
UNSET_MAC = (f'c=$({PB} env unset {PROBE} --json 2>/dev/null | {TOKEN_SED});'
             f' [ -n "$c" ] && {PB} env unset {PROBE} --json --confirm "$c" >/dev/null 2>&1')

MARKER = '# paperboat-env-hook'


def ssh(host, command, stdin=None, tty=False, timeout=120):
    args = ['ssh', '-o', 'BatchMode=yes'] + (['-tt'] if tty else []) + [host]
    if command is not None:
        args.append(command)
    return subprocess.run(args, input=stdin, capture_output=True, text=True, timeout=timeout)


def account(host, command, stdin=None):
    """Run an account command (env set/unset). A Mac's login keychain, which holds the CLI token, is
    locked in every new ssh session, so a controller that is the Mac itself runs them locally."""
    if host in ('localhost', '127.0.0.1'):
        return subprocess.run(['/bin/sh', '-c', command], input=stdin, capture_output=True, text=True, timeout=120)
    return ssh(host, command, stdin=stdin)


def must(result, step):
    if result.returncode:
        raise RuntimeError(f'{step} failed (exit {result.returncode})')
    return result.stdout


def host_is_mac(host):
    return must(ssh(host, 'uname -s'), 'detect the host OS').strip() == 'Darwin'


def probe_hash_line(output):
    for line in output.splitlines():
        if line.startswith('PROBE '):
            return line.split()[1]
    return None


def passed(report):
    return bool(report['noninteractive'] and report['interactive'] and report['cached_ms'] < 500
                and report['env_load'] and report['env_run'] and report['env_run_exit_code']
                and report['env_names'] and report['env_names_scope']
                and report['env_names_off_refused'] and report['env_names_off_run_still_works']
                and report['env_names_back_on']
                and report['quiet_output_bytes'] == 0 and report['quiet_ms'] < 3000
                and report['uninstall_restores_rc'])


# ---- a Windows device ---------------------------------------------------------------------------------------------

# Prints `PROBE <sha256 of the variable>`; the text never holds "PROBE " itself, because a console echoes what is typed.
PS_SHOW = ("[Console]::Out.WriteLine('PRO'+'BE '+(-join([Security.Cryptography.SHA256]::Create().ComputeHash("
           "[Text.Encoding]::UTF8.GetBytes([string]$env:%s))|ForEach-Object{$_.ToString('x2')})))" % PROBE)
DOCUMENTS = "[Console]::Out.Write([Environment]::GetFolderPath('MyDocuments'))"
# `<cached ms> <chars without a runtime> <chars without a login> <ms of those two>` of `pb env hook --emit powershell`,
# timed on the device; stdout and stderr are both counted and never leave it.
EMIT_TIMING = r"""
function Emit($name,$value){$i=New-Object Diagnostics.ProcessStartInfo;$i.FileName=$req.exe
$i.Arguments='env hook --emit powershell';$i.UseShellExecute=$false
$i.RedirectStandardOutput=$true;$i.RedirectStandardError=$true
if($name){$i.EnvironmentVariables[$name]=$value}
$p=[Diagnostics.Process]::Start($i);$e=$p.StandardError.ReadToEndAsync();$o=$p.StandardOutput.ReadToEnd();$p.WaitForExit()
$o.Length+$e.Result.Length}
[void](Emit);$w=[Diagnostics.Stopwatch]::StartNew();[void](Emit);$c=$w.ElapsedMilliseconds
$w.Restart();$a=Emit 'PAPERBOAT_RUNTIME_STATE_ROOT' $req.missing;$b=Emit 'PAPERBOAT_CONFIG' (Join-Path $req.missing 'config.json')
[Console]::Out.Write("$c $a $b $($w.ElapsedMilliseconds)")
"""


def probe_hash(output):
    found = re.findall(r'PROBE ([0-9a-f]{64})', output)
    return found[-1] if found else None


def windows_shell(device, host, script, timeout=120):
    """`ssh HOST powershell ...` with the profile loaded: the plain ssh command of a Windows host."""
    command = ('powershell -NonInteractive -OutputFormat Text -EncodedCommand '
               + base64.b64encode(script.encode('utf-16-le')).decode())
    return subprocess.run(device.ssh_prefix(host, command), stdin=subprocess.DEVNULL, capture_output=True, text=True,
                          timeout=timeout, env=device.ssh_environment())


def windows_typed(device, host, line):
    """An interactive `ssh -tt HOST powershell` (profile loaded): type line, which prints PROBE, then exit.
    Returns what the console showed."""
    from terminal_lifecycle import Terminal
    terminal = Terminal(device.ssh_prefix('-tt', host, 'powershell'),
                        env={'SSHPASS': device.password} if device.password else {})
    try:
        terminal.expect('PS ', 90)
        terminal.expect('>')
        terminal.send(line + '\r')  # Enter is a carriage return in a Windows console
        terminal.expect('PROBE ', 90)
        terminal.send('exit\r')
        terminal.wait_exit(30)
    finally:
        terminal.close()
    return re.sub(r'\x1b\[[0-9;?]*[A-Za-z]', '', bytes(terminal.output).decode(errors='replace'))


def windows_profiles(device):
    documents = winhost.RemotePath(device, device.ps('find the Documents folder', DOCUMENTS)[1])
    return [documents / 'WindowsPowerShell' / 'profile.ps1', documents / 'PowerShell' / 'profile.ps1']


def read_or_empty(path):
    """The file's bytes; a missing file is empty, as `cat rc 2>/dev/null` is on Unix."""
    try:
        return path.read_bytes()
    except FileNotFoundError:
        return b''


def windows_pb(*arguments, stdin=None):
    return winhost.pb_text(winhost.pb_path(), arguments, input=stdin, timeout=120)


def windows_unlock(password_file):
    if password_file:
        must(windows_pb('env', 'vault', 'unlock', '--password-file', password_file, '--json'), 'unlock the ENV vault')


def windows_cleanup(*steps):
    """Every step runs; a cleanup error never hides the check's own error."""
    for step in steps:
        with contextlib.suppress(Exception):
            step()


def windows_unset():
    preview = windows_pb('env', 'unset', PROBE, '--json')  # exits non-zero: confirmation_required
    token = (json.loads(preview.stdout or '{}').get('data') or {}).get('confirmation_token')
    if token:
        windows_pb('env', 'unset', PROBE, '--json', '--confirm', token)


def main_windows(host, device, password_file):
    value = secrets.token_urlsafe(24)
    want = hashlib.sha256(value.encode()).hexdigest()
    report = {'host': host}
    child = ('env', 'run', '--', *windows_target.ps_argv(PS_SHOW))  # powershell -NoProfile: only `env run` can give it
    windows_unlock(password_file)
    profiles = windows_profiles(device)
    before = [read_or_empty(path) for path in profiles]
    try:
        must(windows_pb('env', 'set', PROBE, '--value-stdin', stdin=value), 'env set')
        for _ in range(2):
            must(windows_pb('env', 'hook', 'powershell', '--install'), 'hook install')
        report['hook_lines'] = [sum(line.rstrip().endswith(MARKER.encode()) for line in read_or_empty(path).splitlines())
                                for path in profiles]
        assert report['hook_lines'] == [1, 1], 'install is not idempotent'
        report['noninteractive'] = probe_hash(must(windows_shell(device, host, PS_SHOW), 'ssh host powershell')) == want
        report['interactive'] = probe_hash(windows_typed(device, host, PS_SHOW)) == want
        load = windows_typed(device, host, f'Remove-Item Env:{PROBE}; pb env load *>$null; {PS_SHOW}')
        report['env_load'] = probe_hash(load) == want
        report['env_run'] = probe_hash(must(windows_pb(*child), 'env run')) == want
        listed = must(windows_pb('env', 'names'), 'env names')
        scoped = json.loads(must(windows_pb('env', 'names', '--json'), 'env names --json'))['data']['names']
        report['env_names'] = PROBE in listed.split() and value not in listed
        report['env_names_scope'] = {'name': PROBE, 'scope': 'personal'} in scoped
        must(windows_pb('env', 'names', 'access', 'off'), 'env names access off')
        refused = windows_pb('env', 'names')
        still = must(windows_pb(*child), 'env run with names off')
        report['env_names_off_refused'] = (refused.returncode != 0 and 'turned off' in refused.stderr
                                           and PROBE not in refused.stdout + refused.stderr)
        report['env_names_off_run_still_works'] = probe_hash(still) == want
        must(windows_pb('env', 'names', 'access', 'on'), 'env names access on')
        report['env_names_back_on'] = PROBE in must(windows_pb('env', 'names'), 'env names again').split()
        report['env_run_exit_code'] = windows_pb('env', 'run', '--', 'cmd', '/c', 'exit', '7').returncode == 7
        missing = str(winhost.RemotePath(device, device.env('TEMP')) / ('pb-smoke-missing-' + secrets.token_hex(6)))
        cached, no_runtime, no_login, quiet_ms = map(int, device.ps(
            'time the hook', EMIT_TIMING, exe=device.pb_exe, missing=missing)[1].split())
        report['cached_ms'] = cached
        report['quiet_output_bytes'] = no_runtime + no_login
        report['quiet_ms'] = quiet_ms
        must(windows_pb('env', 'hook', '--uninstall'), 'uninstall')
        report['uninstall_restores_rc'] = [read_or_empty(path) for path in profiles] == before
        report['ok'] = passed(report)
    finally:
        windows_cleanup(lambda: windows_pb('env', 'hook', '--uninstall'),
                        lambda: windows_pb('env', 'names', 'access', 'on'), windows_unset)
    report['status'] = 'PASS' if report.get('ok') else 'FAIL'  # what windows_checks.py reads
    print(json.dumps(report, sort_keys=True))
    return 0 if report.get('ok') else 1


def arguments():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('host')
    parser.add_argument('--password-file', help="Windows device only: owner-only file there with the ENV vault password")
    args = parser.parse_args()
    device = winhost.device()
    if device and args.host != device.target:
        parser.error('SSH_HOST must be the Windows device (SMOKE_WINDOWS_SSH)')
    if args.password_file and not device:
        parser.error('--password-file is for the Windows device; unlock the vault on a Unix host first')
    return args.host, device, args.password_file


def main():
    host, device, password_file = arguments()
    if device:
        return main_windows(host, device, password_file)
    value = secrets.token_urlsafe(24)
    want = hashlib.sha256(value.encode()).hexdigest()
    report = {'host': host}
    mac = host_is_mac(host)
    shell, rc, sha = ('zsh', '~/.zshenv', 'shasum -a 256') if mac else ('bash', '~/.bashrc', 'sha256sum')
    now, ms = ("perl -MTime::HiRes=time -e 'printf(\"%d\", time()*1000)'", '') if mac else ('date +%s%N', '/1000000')
    rc_before = must(ssh(host, f'cat {rc} 2>/dev/null; true'), 'read shell rc file')
    try:
        must(account(host, f'{PB} env set {PROBE} --value-stdin', stdin=value), 'env set')
        must(ssh(host, f'{PB} env hook {shell} --install && {PB} env hook {shell} --install'), 'hook install')
        count = int(must(ssh(host, f'grep -c -- "{MARKER}" {rc}'), 'count').strip())
        report['hook_lines'] = count
        assert count == 1, 'install is not idempotent'
        show = f'printf "PROBE %s\\n" "$(printf %s "${PROBE}" | {sha} | cut -d" " -f1)"'
        report['noninteractive'] = probe_hash_line(must(ssh(host, show), 'ssh host cmd')) == want
        interactive = ssh(host, None, stdin=show + '\nexit\n', tty=True)
        report['interactive'] = probe_hash_line(interactive.stdout.replace('\r', '')) == want
        load = ssh(host, None, stdin=f'unset {PROBE}; pb env load >/dev/null 2>&1; {show}\nexit\n', tty=True)
        report['env_load'] = probe_hash_line(load.stdout.replace('\r', '')) == want
        injected = must(ssh(host, f"unset {PROBE}; {PB} env run -- sh -c '{show}'"), 'env run')
        report['env_run'] = probe_hash_line(injected) == want
        # `pb env names` needs no vault password: the runtime lists names (and layers), never values.
        listed = must(ssh(host, f'{PB} env names'), 'env names')
        scoped = json.loads(must(ssh(host, f'{PB} env names --json'), 'env names --json'))['data']['names']
        report['env_names'] = PROBE in listed.split() and value not in listed
        report['env_names_scope'] = {'name': PROBE, 'scope': 'personal'} in scoped
        # The workspace setting: off refuses the listing with a message and prints no name, `env run` still
        # delivers the value, on lists again. `pb env names access` is the CLI way to flip it.
        must(account(host, f'{PB} env names access off'), 'env names access off')
        refused = ssh(host, f'{PB} env names')
        still = must(ssh(host, f"unset {PROBE}; {PB} env run -- sh -c '{show}'"), 'env run with names off')
        report['env_names_off_refused'] = (refused.returncode != 0 and 'turned off' in refused.stderr
                                           and PROBE not in refused.stdout + refused.stderr)
        report['env_names_off_run_still_works'] = probe_hash_line(still) == want
        must(account(host, f'{PB} env names access on'), 'env names access on')
        report['env_names_back_on'] = PROBE in must(ssh(host, f'{PB} env names'), 'env names again').split()
        passed = ssh(host, f"{PB} env run -- sh -c 'exit 7'; echo rc=$?").stdout
        report['env_run_exit_code'] = 'rc=7' in passed
        emit = f'{PB} env hook --emit {shell} >/dev/null'
        timing = must(ssh(host, f'{emit}; s=$({now}); {emit}; e=$({now}); echo $(((e-s){ms}))'),
                      'timing')
        report['cached_ms'] = int(timing.strip().splitlines()[-1])
        quiet = (f's=$({now}); a=$(PAPERBOAT_RUNTIME_STATE_ROOT=/nonexistent {PB} env hook --emit {shell} 2>&1);'
                 f' b=$(PAPERBOAT_CONFIG=/nonexistent/config.json {PB} env hook --emit {shell} 2>&1);'
                 f' e=$({now}); echo "${{#a}} ${{#b}} $(((e-s){ms}))"')
        no_runtime, no_login, quiet_ms = map(int, must(ssh(host, quiet), 'quiet paths').split())
        report['quiet_output_bytes'] = no_runtime + no_login
        report['quiet_ms'] = quiet_ms
        must(ssh(host, f'{PB} env hook --uninstall'), 'uninstall')
        rc_after = must(ssh(host, f'cat {rc} 2>/dev/null; true'), 'read shell rc file')
        report['uninstall_restores_rc'] = rc_after == rc_before
        report['ok'] = passed(report)
    finally:
        ssh(host, f'{PB} env hook --uninstall >/dev/null 2>&1')
        account(host, f'{PB} env names access on >/dev/null 2>&1')
        account(host, UNSET_MAC if mac else UNSET)
    print(json.dumps(report, sort_keys=True))
    return 0 if report.get('ok') else 1


if __name__ == '__main__':
    sys.exit(main())
