#!/usr/bin/env python3
"""Windows lifecycle checks on victus, driven from a Linux host over SSH (PowerShell), with hp as the peer client.

  VICTUS_PASS=... windows_lifecycle.py --expect-version V --windows-install-json FILE CHECK...
  CHECK: version doctor status service crash availability exec reset-pair uninstall
  reset-pair and uninstall need --token-from HOST (a paired Linux machine that mints enrollment tokens).

Prints one `PASS|FAIL id seconds detail` line per check and exits non-zero on any failure. Never prints tokens,
passwords or file contents.
"""
import argparse
import base64
import json
import os
import re
import secrets
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
SSH = ['-o', 'ConnectTimeout=15', '-o', 'ServerAliveInterval=20', '-o', 'StrictHostKeyChecking=accept-new']
PB_HP = '$HOME/.local/bin/pb'
TOKEN_FILE = r'C:\Users\pujan\pbtok.txt'  # owner-only token file the pairing commands read (removed after use)


def victus(script, timeout=180):
    """Run PowerShell on victus; return (returncode, text)."""
    script = "$ProgressPreference='SilentlyContinue'; " + script
    encoded = base64.b64encode(script.encode('utf-16-le')).decode()
    env = dict(os.environ, SSHPASS=os.environ['VICTUS_PASS'])
    result = subprocess.run(['sshpass', '-e', 'ssh', *SSH, 'pujan@victus', f'powershell -NoProfile -NonInteractive -EncodedCommand {encoded}'],
                            capture_output=True, text=True, timeout=timeout, env=env, stdin=subprocess.DEVNULL)
    text = '\n'.join(line for line in (result.stdout + result.stderr).splitlines() if not line.startswith(('#< CLIXML', '<Objs')))
    return result.returncode, text


def hp(command, timeout=120):
    result = subprocess.run(['ssh', *SSH, '-o', 'BatchMode=yes', 'anvit@hp', command], capture_output=True, text=True,
                            timeout=timeout, stdin=subprocess.DEVNULL)
    return result.returncode, result.stdout + result.stderr


class Windows:
    def __init__(self, pb):
        self.pb = pb

    def run(self, *args, timeout=180):
        quoted = ' '.join("'" + a.replace("'", "''") + "'" for a in args)
        code, out = victus(f"& '{self.pb}' {quoted} 2>&1 | Out-String -Width 220; exit $LASTEXITCODE", timeout)
        return code, out

    def json(self, *args, timeout=180):
        code, out = self.run(*args, '--json', timeout=timeout)
        try:
            return code, json.loads(out[out.index('{'):])
        except ValueError:
            raise RuntimeError(f'{" ".join(args[:2])}: no JSON receipt (rc={code})') from None


def service_pid(name_prefix):
    code, out = victus(f"$s = Get-CimInstance Win32_Service | Where-Object {{ $_.Name -like '{name_prefix}*' }} | Select-Object -First 1; "
                       "\"$($s.Name) $($s.ProcessId) $($s.State)\"")
    name, pid, state = (out.split() + ['', '0', ''])[:3]
    return name, int(pid or 0), state


def wait_for(predicate, seconds, what):
    deadline = time.time() + seconds
    while time.time() < deadline:
        if predicate():
            return
        time.sleep(3)
    raise RuntimeError(f'timed out waiting for {what}')


def mint_token(host, alias):
    """Mint an enrollment token on a paired Linux machine, stage it on victus owner-only; return nothing printed."""
    code, out = hp(f'{PB_HP} machine add --json --name {alias}') if host == 'hp' else (1, '')
    if code:
        raise RuntimeError('could not mint an enrollment token on ' + host)
    command = json.loads(out)['data']['windows_command']
    token = re.search(r"p=([^'\"&]+)", command).group(1)[-26:]
    code, _ = victus(
        f"Set-Content -NoNewline -Path '{TOKEN_FILE}' -Value '{token}'; "
        f"icacls '{TOKEN_FILE}' /setowner pujan | Out-Null; icacls '{TOKEN_FILE}' /inheritance:r /grant:r 'pujan:F' 'SYSTEM:F' | Out-Null; "
        f"icacls '{TOKEN_FILE}' /remove Administrators | Out-Null")
    if code:
        raise RuntimeError('could not stage the enrollment token on victus')


def check_version(win, args):
    code, out = win.run('--version')
    assert args.expect_version in out, 'version differs: ' + out.strip().splitlines()[-1][:80]


def check_doctor(win, args):
    code, out = win.run('doctor')
    assert 'doctor: healthy' in out, ' '.join(re.findall(r'^\w+: (?:fail|warning)[^\n]*', out, re.M))[:200]


def check_status(win, args):
    code, out = win.run('status')
    assert code == 0 and 'Daemon: ready' in out, f'status rc={code}: ' + ' '.join(out.split())[:100]


def check_service(win, args):
    """C013-C019: status, stop, start, restart, then doctor healthy."""
    def state():
        return win.run('service', 'status')[1].strip().lower()
    for action, expected in (('stop', 'stopped'), ('start', 'running'), ('restart', 'running')):
        code, out = win.run('service', action)
        assert code == 0, f'service {action} rc={code}: {out.strip()[:120]}'
        wait_for(lambda: expected in state(), 60, f'service {action} -> {expected} (now: {state()[:60]})')
    check_doctor(win, args)


def check_crash(win, args):
    """F1-11: kill the daemon service process; the service manager restarts it and doctor is healthy again."""
    name, pid, _ = service_pid('PaperboatLocalDaemon')
    assert pid, 'daemon service has no process'
    victus(f'Stop-Process -Id {pid} -Force')
    wait_for(lambda: service_pid('PaperboatLocalDaemon')[1] not in (0, pid), 120, 'the daemon service to restart under a new process')
    wait_for(lambda: 'doctor: healthy' in win.run('doctor')[1], 120, 'doctor healthy after the crash')


def check_availability(win, args):
    """F2-06 / C032: keep-awake then allow-sleep from the CLI show up as a Windows power request on the device."""
    def keep_awake_requested():
        return 'paperboat' in victus('powercfg /requests | Out-String')[1].lower()

    def set_mode(mode):
        base = f'{PB_HP} machine availability {args.machine} --mode {mode} --json'
        if mode == 'keep-awake':  # the CLI first asks for its confirmation code (exit 2), then takes it with --confirm
            code, out = hp(base)
            token = json.loads(out)['data']['confirmation_token']
            base += f' --confirm {token}'
        code, out = hp(base)
        assert code == 0, f'availability {mode}: {out.strip()[:140]}'
    for mode, wanted in (('keep-awake', True), ('allow-sleep', False)):
        set_mode(mode)
        wait_for(lambda: keep_awake_requested() == wanted, 90, f'powercfg request present={wanted} after {mode}')


def check_exec(win, args):
    marker = 'pbwin-' + secrets.token_hex(4)
    code, out = hp(f"{PB_HP} exec {args.machine} -- cmd /c echo {marker}")
    assert code == 0 and marker in out, f'exec rc={code}: {out.strip()[-120:]}'


def check_reset_pair(win, args):
    """F1-10: pb reset + pair with a new token on the same machine; the old record is replaced; doctor healthy."""
    mint_token(args.token_from, args.machine)
    host = victus('hostname')[1].strip()
    try:
        code, receipt = win.json('reset', '--confirmation', 'RESET PAPERBOAT', '--hostname', host, '--enrollment-token-file', TOKEN_FILE, timeout=300)
        assert code == 0 and receipt.get('data', {}).get('reset'), 'reset failed: ' + json.dumps(receipt.get('error', {}))[:160]
        code, receipt = win.json('pair', '--enrollment-token-file', TOKEN_FILE, '--name', args.machine, timeout=420)
        assert code == 0 and receipt.get('ok'), 'pair failed: ' + json.dumps(receipt.get('error', {}))[:200]
    finally:
        victus(f"Remove-Item -Force '{TOKEN_FILE}' -ErrorAction SilentlyContinue")
    machine_id = receipt['data']['machine_id']
    wait_for(lambda: 'doctor: healthy' in win.run('doctor')[1], 180, 'doctor healthy after re-pair')
    wait_for(lambda: re.search(rf'\sonline\s.*{re.escape(machine_id)}', hp(f'{PB_HP} machine list')[1]), 120, 'the new machine record online in the account')
    args.machine = re.search(rf'^(\S+)\s.*{re.escape(machine_id)}', hp(f'{PB_HP} machine list')[1], re.M).group(1)  # the pair may suffix a taken name


def check_uninstall(win, args):
    """F1-09: pb uninstall removes everything: the Windows inventory finds nothing left."""
    host = victus('hostname')[1].strip()
    code, out = win.run('uninstall', '--confirmation', 'UNINSTALL PAPERBOAT', '--hostname', host, timeout=600)
    assert code == 0, f'uninstall rc={code}: {out.strip()[-200:]}'
    inventory(args)


def inventory(args):
    script = open(os.path.join(HERE, 'uninstall_inventory_windows.ps1')).read()
    env = dict(os.environ, SSHPASS=os.environ['VICTUS_PASS'])
    subprocess.run(['sshpass', '-e', 'scp', '-q', *SSH, os.path.join(HERE, 'uninstall_inventory_windows.ps1'), r'pujan@victus:C:/Users/pujan/pb-inventory.ps1'],
                   check=True, env=env)
    code, out = victus(r'powershell -NoProfile -ExecutionPolicy Bypass -File C:\Users\pujan\pb-inventory.ps1; exit $LASTEXITCODE', 300)
    victus(r"Remove-Item -Force C:\Users\pujan\pb-inventory.ps1 -ErrorAction SilentlyContinue")
    assert code == 0 and 'inventory: clean' in out, 'leftovers: ' + ' | '.join(l for l in out.splitlines() if l.startswith('leftover'))[:400]


CHECKS = {'version': check_version, 'doctor': check_doctor, 'status': check_status, 'service': check_service, 'crash': check_crash,
          'availability': check_availability, 'exec': check_exec, 'reset-pair': check_reset_pair, 'uninstall': check_uninstall}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--expect-version', required=True)
    parser.add_argument('--windows-install-json', required=True)
    parser.add_argument('--machine', default='victus', help='machine name in the account')
    parser.add_argument('--token-from', default='hp')
    parser.add_argument('checks', nargs='+', choices=sorted(CHECKS))
    args = parser.parse_args()
    if not os.environ.get('VICTUS_PASS'):
        sys.exit('export VICTUS_PASS (see REMOTE_TESTING.md)')
    from install_path import windows_executable
    win = Windows(windows_executable(json.load(open(args.windows_install_json))))
    failed = False
    for name in args.checks:
        started = time.time()
        try:
            CHECKS[name](win, args)
            status, detail = 'PASS', ''
        except (AssertionError, RuntimeError, subprocess.SubprocessError) as error:
            status, detail, failed = 'FAIL', str(error), True
        print(f'{status} windows:{name} {time.time() - started:.0f}s {detail}', flush=True)
    sys.exit(1 if failed else 0)


if __name__ == '__main__':
    main()
