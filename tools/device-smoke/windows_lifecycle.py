#!/usr/bin/env python3
"""Windows lifecycle checks on the Windows device, driven from a Linux host over SSH (PowerShell), with hp as the peer
client. The device is SMOKE_WINDOWS_SSH (default pujan@victus, password VICTUS_PASS; any other target logs in by key);
the peer is SMOKE_PEER_SSH (default anvit@hp), a paired Unix machine of the same account.

  VICTUS_PASS=... windows_lifecycle.py --expect-version V --windows-install-json FILE CHECK...
  CHECK: install-pair update update-failures version doctor status service crash availability exec exec-self reset-pair uninstall
  install-pair and update need --build (the new pb.exe already copied to victus); install-pair writes the install receipt to the
  --windows-install-json file. install-pair and reset-pair mint tokens on hp. Results go to results/windows-lifecycle-<UTC>.json.

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

import pairing_lifecycle
import winhost

HERE = os.path.dirname(os.path.abspath(__file__))
SSH = ['-o', 'ConnectTimeout=15', '-o', 'ServerAliveInterval=20', '-o', 'StrictHostKeyChecking=accept-new']
DEVICE = winhost.WindowsDevice()
PB_HP = '$HOME/.local/bin/pb'
PEER = os.environ.get('SMOKE_PEER_SSH', 'anvit@hp')


def doctor_ok(out):
    """healthy, or degraded only by the hosted runner's own HTTP.sys holding port 80/443 (the same exact warning pairing_lifecycle tolerates)."""
    if 'doctor: healthy' in out:
        return True
    problems = re.findall(r'^\S+: (?:fail|warning)[^\n]*', out, re.M)
    return 'doctor: degraded' in out and bool(problems) and all(pairing_lifecycle.HTTP_SYS_PORT_WARNING in line for line in problems)


def home(name):
    return str(DEVICE.home() / name)


def victus(script, timeout=180):
    """Run PowerShell on victus; return (returncode, text)."""
    script = "$ProgressPreference='SilentlyContinue'; " + script
    encoded = base64.b64encode(script.encode('utf-16-le')).decode()
    result = subprocess.run([*DEVICE.ssh_prefix(), DEVICE.target, f'powershell -NoProfile -NonInteractive -EncodedCommand {encoded}'],
                            capture_output=True, text=True, timeout=timeout, env=DEVICE.ssh_environment(), stdin=subprocess.DEVNULL)
    text = '\n'.join(line for line in (result.stdout + result.stderr).splitlines() if not line.startswith(('#< CLIXML', '<Objs')))
    return result.returncode, text


def hp(command, timeout=120):
    result = subprocess.run(['ssh', *SSH, '-o', 'BatchMode=yes', PEER, command], capture_output=True, text=True,
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
        f"icacls '{TOKEN_FILE}' /setowner $env:USERNAME | Out-Null; "
        f"icacls '{TOKEN_FILE}' /inheritance:r /grant:r \"$($env:USERNAME):F\" 'SYSTEM:F' | Out-Null; "
        f"icacls '{TOKEN_FILE}' /remove Administrators | Out-Null")
    if code:
        raise RuntimeError('could not stage the enrollment token on victus')


def machine_named(machine_id):
    found = re.search(rf'^(\S+)\s.*{re.escape(machine_id)}', hp(f'{PB_HP} machine list --all')[1], re.M)
    return found.group(1) if found else None


def check_install_pair(win, args):
    """F1-01/F1-02 (custom build): the build's own `pb install`, then `pb pair`; online and healthy within 30 s."""
    assert args.build, 'install-pair needs --build <pb.exe on victus>'
    code, out = victus(f"& '{args.build}' install --json 2>&1 | Out-String -Width 4000; exit $LASTEXITCODE", 600)
    receipt = json.loads(out[out.index('{'):])
    assert code == 0 and receipt.get('ok'), 'install failed: ' + json.dumps(receipt.get('error', {}))[:200]
    with open(args.windows_install_json, 'w') as saved:
        json.dump(receipt, saved)
    win.pb = receipt['data']['executable']
    mint_token(args.token_from, args.machine)
    try:
        code, receipt = win.json('pair', '--enrollment-token-file', TOKEN_FILE, '--name', args.machine, timeout=420)
        assert code == 0 and receipt.get('ok'), 'pair failed: ' + json.dumps(receipt.get('error', {}))[:200]
    finally:
        victus(f"Remove-Item -Force '{TOKEN_FILE}' -ErrorAction SilentlyContinue")
    paired, machine_id = time.time(), receipt['data']['machine_id']
    # Audit C #26: pair returns only once the local daemon serves (the elevated install waits for it), so the first
    # status right after it, with no polling, already says ready.
    status = win.run('status')
    assert status[0] == 0 and 'Daemon: ready' in status[1], 'the daemon was not ready when pair returned: ' + ' '.join(status[1].split())[:100]
    wait_for(lambda: re.search(rf'\sonline\s.*{re.escape(machine_id)}', hp(f'{PB_HP} machine list --all')[1]), 30, 'the machine online in the account within 30 s of pairing')
    wait_for(lambda: doctor_ok(win.run('doctor')[1]), max(1, 60 - (time.time() - paired)), 'doctor healthy within 60 s of pairing')
    args.machine = machine_named(machine_id)  # the pair may suffix a taken name


NET_SECURITY = r'C:\Windows\System32\WindowsPowerShell\v1.0\Modules\NetSecurity'


def check_pair_firewall_failure(win, args):
    """F1-01: a first pair whose firewall step keeps failing ends in a clear, retryable error, never a stuck install;
    after the firewall works again the same pair completes without a new token. Needs an elevated shell (hosted runner)."""
    assert args.build, 'pair-firewall-failure needs --build <pb.exe on the device>'
    code, out = victus(f"& '{args.build}' install --json 2>&1 | Out-String -Width 4000; exit $LASTEXITCODE", 600)
    receipt = json.loads(out[out.index('{'):])
    assert code == 0 and receipt.get('ok'), 'install failed: ' + json.dumps(receipt.get('error', {}))[:200]
    win.pb = receipt['data']['executable']
    mint_token(args.token_from, args.machine)
    # The firewall cmdlets live in NetSecurity: without it the snapshot fails on every retry like a broken ActiveStore read.
    victus(f"Rename-Item -Path '{NET_SECURITY}' -NewName NetSecurity.pbtest")
    try:
        code, receipt = win.json('pair', '--enrollment-token-file', TOKEN_FILE, '--name', args.machine, timeout=420)
    finally:
        victus(f"Remove-Item -Force '{TOKEN_FILE}' -ErrorAction SilentlyContinue; "
               f"if (Test-Path '{NET_SECURITY}.pbtest') {{ Rename-Item -Path '{NET_SECURITY}.pbtest' -NewName NetSecurity }}")
    assert code != 0 and receipt.get('ok') is False, 'pair succeeded although the firewall step could not run'
    assert receipt['error'].get('message'), 'the failed pair gave no message'
    start_type = victus("(Get-Service | Where-Object { $_.Name -like 'PaperboatLocalDaemon-*' } | Select-Object -First 1).StartType")[1].strip()
    assert start_type != 'Disabled', 'the failed pair left the local daemon disabled'
    code, receipt = win.json('pair', '--name', args.machine, timeout=420)
    assert code == 0 and receipt.get('ok'), 'the retry without a token failed: ' + json.dumps(receipt.get('error', {}))[:200]
    machine_id = receipt['data']['machine_id']
    wait_for(lambda: re.search(rf'\sonline\s.*{re.escape(machine_id)}', hp(f'{PB_HP} machine list --all')[1]), 60, 'the machine online after the retry')
    wait_for(lambda: doctor_ok(win.run('doctor')[1]), 90, 'doctor healthy after the retry')
    args.machine = machine_named(machine_id)


def check_update(win, args):
    """F14-01: pb update --from <newer build> succeeds, reports the new version, and leaves one healthy generation."""
    assert args.build, 'update needs --build <newer pb.exe on victus>'
    code, receipt = win.json('update', '--from', args.build, timeout=600)
    data = receipt.get('data', {})
    assert code == 0 and receipt.get('ok'), 'update failed: ' + json.dumps(receipt.get('error', {}))[:200]
    assert data.get('changed') is True and data.get('version') == args.expect_version, f"update reported {data.get('previous_version')} -> {data.get('version')}"
    check_version(win, args)
    wait_for(lambda: doctor_ok(win.run('doctor')[1]), 120, 'doctor healthy after the update')
    count = "(Get-Service | Where-Object { $_.Name -like 'PaperboatSessionOwner-*' -or $_.Name -like 'PaperboatSshd-*' } | Measure-Object).Count"
    wait_for(lambda: victus(count)[1].strip() == '2', 120, 'only the new session-owner and SSH generation left')


def check_update_failures(win, args):
    """F14-03: a bad, truncated or wrong-platform file is refused and a killed update recovers; the install keeps working."""
    assert args.build and args.wrong_platform, 'update-failures needs --build <good pb.exe> and --wrong-platform <non-Windows pb> on victus'
    original = installed_version(win)
    good_version = installed_version(Windows(args.build))
    work = home('pb-update-fail')

    def healthy(seconds):
        wait_for(lambda: doctor_ok(win.run('doctor')[1]), seconds, 'doctor healthy')
        check_exec(win, args)
    victus(f"New-Item -ItemType Directory -Force '{work}' | Out-Null; "
           f"$b = New-Object byte[] 4096; (New-Object Random).NextBytes($b); [IO.File]::WriteAllBytes('{work}\\garbage.exe', $b); "
           f"$g = [IO.File]::ReadAllBytes('{args.build}'); [IO.File]::WriteAllBytes('{work}\\truncated.exe', $g[0..([int]($g.Length / 3))]); "
           f"Copy-Item '{args.wrong_platform}' '{work}\\wrong-platform.exe' -Force", 120)
    try:
        for name in ('garbage', 'truncated', 'wrong-platform'):
            code, receipt = win.json('update', '--from', f'{work}\\{name}.exe', timeout=300)
            assert code != 0 and receipt.get('ok') is False, f'{name} update was accepted'
            assert receipt['error'].get('message'), f'{name} update was refused without a message'
            assert installed_version(win) == original, f'{name} update changed the installed version'
            healthy(150)
        # Killed mid-update: the previous or the new version must work afterwards, and a retry completes.
        victus(f"$p = Start-Process -FilePath '{win.pb}' -ArgumentList 'update','--from','{args.build}','--force','--json' -PassThru -WindowStyle Hidden; "
               "Start-Sleep 20; taskkill /PID $p.Id /T /F | Out-Null; "
               "Get-Process | Where-Object { $_.Name -like '.pb-elevated-*' } | ForEach-Object { taskkill /PID $_.Id /T /F | Out-Null }", 120)
        healthy(300)
        after_kill = installed_version(win)
        assert after_kill in (original, good_version), f'a killed update left version {after_kill}'
        deadline = time.time() + 180
        while True:
            code, receipt = win.json('update', '--from', args.build, timeout=600)
            if code == 0:
                break
            busy = receipt.get('error', {}).get('code') == 'update_busy'
            assert busy and time.time() < deadline, 'the retried update failed: ' + json.dumps(receipt.get('error', {}))[:200]
            time.sleep(5)
        healthy(300)
        assert installed_version(win) == good_version, 'the retried update did not install the good build'
    finally:
        victus(f"Remove-Item -Recurse -Force '{work}' -ErrorAction SilentlyContinue")


def check_uninstall_after_failed_update(win, args):
    """F1-09: an uninstall after an update killed mid-way (candidate record and install log left) removes everything."""
    assert args.build, 'uninstall-after-failed-update needs --build <good pb.exe> on victus'
    victus(f"$p = Start-Process -FilePath '{win.pb}' -ArgumentList 'update','--from','{args.build}','--force','--json' -PassThru -WindowStyle Hidden; "
           "Start-Sleep 20; taskkill /PID $p.Id /T /F | Out-Null; "
           "Get-Process | Where-Object { $_.Name -like '.pb-elevated-*' } | ForEach-Object { taskkill /PID $_.Id /T /F | Out-Null }", 120)
    # Not healed: the uninstall starts from the half-updated state.
    return check_uninstall(win, args)


def installed_version(win):
    code, out = win.run('--version', '--json')
    return json.loads(out[out.index('{'):])['data']['version']


def check_version(win, args):
    code, out = win.run('--version')
    assert args.expect_version in out, 'version differs: ' + out.strip().splitlines()[-1][:80]


def check_doctor(win, args):
    code, out = win.run('doctor')
    assert doctor_ok(out), ' '.join(re.findall(r'^\w+: (?:fail|warning)[^\n]*', out, re.M))[:200]


def check_status(win, args):
    code, out = win.run('status')
    assert code == 0 and 'Daemon: ready' in out, f'status rc={code}: ' + ' '.join(out.split())[:100]


def check_service(win, args):
    """C013-C019: status, stop, start, restart, uninstall, install (each of the last two twice: both are idempotent), then doctor healthy."""
    def state():
        return win.run('service', 'status')[1].strip().lower()
    steps = (('stop', 'stopped'), ('start', 'running'), ('restart', 'running'), ('uninstall', 'not-installed'),
             ('uninstall', 'not-installed'), ('install', 'running'), ('install', 'running'))
    try:
        for action, expected in steps:
            code, out = win.run('service', action)
            assert code == 0, f'service {action} rc={code}: {out.strip()[:120]}'
            wait_for(lambda: expected in state(), 60, f'service {action} -> {expected} (now: {state()[:60]})')
            if expected == 'not-installed':
                # Linux reinstalls the user unit from any command; Windows needs administrator rights for that,
                # so a command that needs the daemon fails and names the command that brings it back.
                code, out = win.run('doctor')
                assert code != 0 and 'pb service install' in out, f'doctor without the daemon service rc={code}: {out.strip()[:120]}'
                assert 'not-installed' in state(), 'doctor recreated the uninstalled daemon service'
    finally:
        win.run('service', 'install')  # leave a working daemon even when a step fails
    check_doctor(win, args)


def check_crash(win, args):
    """F1-11: kill the daemon service process; the service manager restarts it and doctor is healthy again."""
    name, pid, _ = service_pid('PaperboatLocalDaemon')
    assert pid, 'daemon service has no process'
    victus(f'Stop-Process -Id {pid} -Force')
    wait_for(lambda: service_pid('PaperboatLocalDaemon')[1] not in (0, pid), 120, 'the daemon service to restart under a new process')
    wait_for(lambda: doctor_ok(win.run('doctor')[1]), 120, 'doctor healthy after the crash')


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


def check_exec_self(win, args):
    """pb exec run on Windows (to this machine itself)."""
    marker = 'pbself-' + secrets.token_hex(4)
    code, out = win.run('exec', args.machine, '--', 'cmd', '/c', 'echo', marker, timeout=90)
    assert code == 0 and marker in out, f'exec on Windows rc={code}: {" ".join(out.split())[-140:]}'


def check_reset_pair(win, args):
    """F1-10: pb reset + pair with a new token on the same machine; the old record is replaced; doctor healthy."""
    old_id = json.loads(victus(r"Get-Content -Raw (Join-Path $env:LOCALAPPDATA 'Paperboat\runtime\machine-registration.json')")[1])['machine_id']
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
    wait_for(lambda: doctor_ok(win.run('doctor')[1]), 180, 'doctor healthy after re-pair')
    wait_for(lambda: re.search(rf'\sonline\s.*{re.escape(machine_id)}', hp(f'{PB_HP} machine list')[1]), 120, 'the new machine record online in the account')
    args.machine = machine_named(machine_id)  # the pair may suffix a taken name
    assert machine_id != old_id and machine_named(old_id) is None, 'the old machine record is still in the account after re-pair'


def check_uninstall(win, args):
    """F1-09: pb uninstall removes everything: the Windows inventory finds nothing left."""
    host = victus('hostname')[1].strip()
    code, out = win.run('uninstall', '--confirmation', 'UNINSTALL PAPERBOAT', '--hostname', host, timeout=600)
    assert code == 0, f'uninstall rc={code}: {out.strip()[-200:]}'
    # The uninstall hands the removal to a helper that outlives the command (services stop after it returns).
    deadline = time.time() + 240
    while True:
        try:
            return inventory(args)
        except AssertionError:
            if time.time() > deadline:
                raise
            time.sleep(10)


def inventory(args):
    script = open(os.path.join(HERE, 'uninstall_inventory_windows.ps1')).read()
    path = DEVICE.home() / 'pb-inventory.ps1'
    path.write_text(script)
    code, out = victus(f"powershell -NoProfile -ExecutionPolicy Bypass -File '{path}'; exit $LASTEXITCODE", 300)
    path.unlink()
    assert code == 0 and 'inventory: clean' in out, 'leftovers: ' + ' | '.join(l for l in out.splitlines() if l.startswith('leftover'))[:400]


CHECKS = {'install-pair': check_install_pair, 'pair-firewall-failure': check_pair_firewall_failure, 'update': check_update, 'update-failures': check_update_failures, 'exec-self': check_exec_self, 'version': check_version, 'doctor': check_doctor, 'status': check_status, 'service': check_service, 'crash': check_crash,
          'availability': check_availability, 'exec': check_exec, 'reset-pair': check_reset_pair, 'uninstall': check_uninstall, 'uninstall-after-failed-update': check_uninstall_after_failed_update}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--expect-version', required=True)
    parser.add_argument('--windows-install-json', required=True)
    parser.add_argument('--machine', default='victus', help='machine name in the account')
    parser.add_argument('--token-from', default='hp')
    parser.add_argument('--build', help='path on victus of the pb.exe to install (install-pair)')
    parser.add_argument('--wrong-platform', help='path on victus of a pb built for another OS (update-failures)')
    parser.add_argument('checks', nargs='+', choices=sorted(CHECKS))
    args = parser.parse_args()
    if DEVICE.target == winhost.DEFAULT_TARGET and not DEVICE.password:
        sys.exit('export VICTUS_PASS (see REMOTE_TESTING.md)')
    global TOKEN_FILE  # owner-only token file the pairing commands read (removed after use)
    TOKEN_FILE = home('pbtok.txt')
    from install_path import windows_executable
    win = Windows(None if args.checks[0] == 'install-pair' else windows_executable(json.load(open(args.windows_install_json))))
    failed, rows = False, []
    for name in args.checks:
        started = time.time()
        try:
            CHECKS[name](win, args)
            status, detail = 'PASS', ''
        except (AssertionError, RuntimeError, subprocess.SubprocessError) as error:
            status, detail, failed = 'FAIL', str(error), True
        print(f'{status} windows:{name} {time.time() - started:.0f}s {detail}', flush=True)
        rows.append({'id': f'windows:{name}', 'status': status, 'seconds': round(time.time() - started), 'detail': detail})
    os.makedirs(os.path.join(HERE, 'results'), exist_ok=True)
    result = os.path.join(HERE, 'results', time.strftime('windows-lifecycle-%Y%m%dT%H%M%SZ.json', time.gmtime()))
    with open(result, 'w') as saved:
        json.dump({'version': args.expect_version, 'device': DEVICE.target, 'results': rows}, saved, indent=1)
    print('results:', os.path.relpath(result, os.path.dirname(HERE)))
    sys.exit(1 if failed else 0)


if __name__ == '__main__':
    main()
