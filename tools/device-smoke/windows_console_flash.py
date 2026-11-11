#!/usr/bin/env python3
"""windows_console_flash.py CASE [EXE ARGS...]: no Paperboat background work may flash a console on the Windows desktop.

CASE runonce  the uninstall RunOnce entry (conhost --headless cmd /c rd) run as Explorer would at sign-in
CASE signin-arm   register a logon task that watches the desktop for 300 s after the next sign-in (run it before `pb uninstall`)
CASE signin-read  read what that watcher saw (out.json and log); fails on any visible console window or a blind watcher
CASE exec     EXE ARGS... (e.g. pb.exe install ..., pb.exe update ..., pb.exe pair ...) started with no console
Each case first runs a control (plain cmd.exe with no console must open a visible window), so a blind detector fails.
SSH runs in session 0 and cannot see the desktop, so the detector (windows_console_flash.ps1) runs through an
interactive scheduled task of the signed-in user (victus has one; a hosted runner does when its console session is Active; set VIA_EXPLORER=1 there). Needs VICTUS_PASS, SMOKE_WINDOWS_SSH.
"""
import json
import os
import sys
import time
import uuid

import winhost

TASK = 'pbsmoke-console-flash'
SIGNIN_TASK = TASK + '-signin'
SIGNIN_DIR = 'C:\\Users\\Public\\pbflash-signin'  # outlives the user's TEMP and survives sign-out
CONSOLE_EXES = ('cmd.exe', 'powershell.exe', 'pwsh.exe')  # opens a visible console when detached: only the control may run one


def run_in_session(device, name, exe, argument_line, seconds, control=False, elevated=False, timeout=0):
    if not control and exe.rsplit('\\', 1)[-1].lower() in CONSOLE_EXES:
        sys.exit('FAIL %s: %s would open a visible console in the user\'s session; run it over ssh instead' % (name, exe))
    work = device.env('TEMP') + '\\pbflash-' + uuid.uuid4().hex[:8]
    try:
        return _run_in_session(device, name, exe, argument_line, seconds, work, control, elevated, timeout)
    finally:  # a failed run must leave no task, window or file in the user's session
        device.ps('unregister', "schtasks /Delete /TN $req.t /F | Out-Null", t=TASK, ok=(0, 1))
        device.ps('cleanup', 'Get-CimInstance Win32_Process | Where-Object { $_.ProcessId -ne $PID -and $_.CommandLine -like ("*" + $req.d + "*") } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue };'
                  'Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $req.d', d=work, ok=(0, 1))


def _run_in_session(device, name, exe, argument_line, seconds, work, control, elevated, timeout):
    device.mkdir(work)
    script, out, done = work + '\\detect.ps1', work + '\\out.json', work + '\\done.txt'
    device.write_bytes(script, open(__file__.replace('.py', '.ps1'), 'rb').read())
    arguments = ['-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', script, '-Exe', exe, '-ArgumentLine', argument_line,
                 '-Seconds', str(seconds), '-Timeout', str(timeout), '-Log', work + '\\child.txt', '-PathPrepend', os.environ.get('FLASH_PATH', '')] + (['-Inherit'] if os.environ.get('FLASH_INHERIT') and not control else []) + [ '-Out', out]
    if control:
        arguments.append('-Control')
    line = 'powershell.exe ' + ' '.join('"%s"' % a.replace('"', '\\"') for a in arguments)
    launcher = work + '\\run.cmd'  # a path without quotes keeps schtasks /TR free of nested quoting
    device.write_bytes(launcher, ('@echo off\r\n' + line + ' > "' + work + '\\log.txt" 2>&1\r\n').encode())
    task = 'C:\\Windows\\System32\\conhost.exe --headless cmd.exe /c ' + launcher  # headless: the harness shows no window itself
    if os.environ.get('VIA_EXPLORER'):  # a hosted runner: Explorer starts the detector outside the task's job object
        # (a task job forbids CREATE_BREAKAWAY_FROM_JOB, which pb's elevation needs). This opens a visible launcher window.
        device.write_bytes(work + '\\launch.cmd', ('@cmd.exe /c ' + launcher + '\r\n').encode())
        task = 'C:\\Windows\\explorer.exe ' + work + '\\launch.cmd'
    device.ps('register ' + name, "$u=[Security.Principal.WindowsIdentity]::GetCurrent().Name;"
              "schtasks /Create /TN $req.t /SC ONCE /ST 00:00 /RU $u /IT /RL $req.l /F /TR $req.c | Out-Null;"
              "if($LASTEXITCODE){throw 'schtasks /Create failed'};schtasks /Run /TN $req.t | Out-Null", t=TASK, l='HIGHEST' if elevated else 'LIMITED', c=task)  # headless: the harness shows no window itself
    deadline = time.time() + max(seconds, timeout) * 3 + 240
    while time.time() < deadline and not device.exists(out):
        time.sleep(2)
    if not device.exists(out):
        log = device.read_bytes(work + '\\log.txt').decode(errors='replace')[-400:] if device.exists(work + '\\log.txt') else 'no log'
        sys.exit('FAIL %s: the desktop detector produced no result (is a user signed in on the device?): %s' % (name, log))
    result = json.loads(device.read_bytes(out).decode('utf-8-sig'))
    if device.exists(work + '\\child.txt'):
        result['output'] = device.read_bytes(work + '\\child.txt').decode(errors='replace')[-1500:]
    return result


def check(device, name, exe, argument_line, seconds=30, elevated=False, timeout=0):
    control = run_in_session(device, name + ' control', 'cmd.exe', '/c ping -n 3 127.0.0.1', 5, control=True)
    if not control['flashes']:
        sys.exit('FAIL %s: the detector saw no window for the control; the check is blind' % name)
    result = run_in_session(device, name, exe, argument_line, seconds, elevated=elevated, timeout=timeout)
    if result['flashes']:
        sys.exit('FAIL %s: windows opened by %s: %s' % (name, result['command'], json.dumps(result['flashes'])))
    print('output: ' + result.get('output', '').strip()[:600].replace('\n', ' | '))
    print('PASS %s: no visible console window for %ss after %s (exit %s)' % (name, seconds, result['command'], result['exit']))


def signin_arm(device):
    """Control (plain cmd.exe must open a window), then watch 300 s from the next sign-in; the result stays on the device."""
    device.ps('mkdir', 'New-Item -ItemType Directory -Force $req.d | Out-Null', d=SIGNIN_DIR)
    device.write_bytes(SIGNIN_DIR + '\\detect.ps1', open(__file__.replace('.py', '.ps1'), 'rb').read())
    line = ('powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%s\\detect.ps1" -Exe cmd.exe -ArgumentLine "/c ping -n 3 127.0.0.1" -Watch -Seconds 300 -Timeout 10'
            ' -Log "%s\\child.txt" -Out "%s\\out.json" > "%s\\log.txt" 2>&1\r\n') % ((SIGNIN_DIR,) * 4)
    device.write_bytes(SIGNIN_DIR + '\\run.cmd', ('@echo off\r\n' + line).encode())
    device.ps('clear', 'foreach ($f in "out.json","log.txt","child.txt") { Remove-Item -Force -ErrorAction SilentlyContinue (Join-Path $req.d $f) }', d=SIGNIN_DIR)
    device.ps('register', "$u=[Security.Principal.WindowsIdentity]::GetCurrent().Name;"
              "schtasks /Create /TN $req.t /SC ONLOGON /RU $u /IT /RL LIMITED /F /TR $req.c | Out-Null;if($LASTEXITCODE){throw 'schtasks /Create failed'}",
              t=SIGNIN_TASK, c='C:\\Windows\\System32\\conhost.exe --headless cmd.exe /c ' + SIGNIN_DIR + '\\run.cmd')
    print('armed: the watcher starts at the next sign-in of the installing user')


def signin_read(device):
    if not device.exists(SIGNIN_DIR + '\\out.json'):
        sys.exit('FAIL signin: no out.json yet (no sign-in since arming, or the 300 s watch has not ended)')
    result = json.loads(device.read_bytes(SIGNIN_DIR + '\\out.json').decode('utf-8-sig'))
    device.ps('disarm', "schtasks /Delete /TN $req.t /F | Out-Null", t=SIGNIN_TASK, ok=(0, 1))
    print(device.read_bytes(SIGNIN_DIR + '\\log.txt').decode(errors='replace').strip())
    if not result.get('watch') or not result.get('control_flashes'):
        sys.exit('FAIL signin: the watcher was blind (no window for its control)')
    if result['flashes']:
        sys.exit('FAIL signin: windows opened after sign-in: %s' % json.dumps(result['flashes']))
    print('PASS signin: no visible console window for 300s after the sign-in that began %s' % result.get('started'))


def main():
    device = winhost.WindowsDevice()  # no pb.exe needed: the check runs on the desktop
    case = sys.argv[1] if len(sys.argv) > 1 else sys.exit(__doc__)
    if case == 'runonce':
        system = device.env('SystemRoot') + '\\System32'
        helper = device.env('TEMP') + '\\Paperboat Uninstall\\' + uuid.uuid4().hex
        device.mkdir(helper)
        check(device, 'runonce', system + '\\conhost.exe', '--headless %s\\cmd.exe /d /c rd /s /q "%s"' % (system, helper), 10)
        if device.exists(helper):
            sys.exit('FAIL runonce: the helper directory was not removed')
    elif case == 'signin-arm':
        signin_arm(device)
    elif case == 'signin-read':
        signin_read(device)
    elif case == 'exec' and len(sys.argv) > 2:  # EXE ARGS...: an administrator's task (HIGHEST: no UAC prompt on the user's desktop)
        check(device, 'exec ' + sys.argv[2].rsplit('\\', 1)[-1] + ' ' + (sys.argv[3] if len(sys.argv) > 3 else ''), sys.argv[2],
              ' '.join(sys.argv[3:]), int(os.environ.get('FLASH_SECONDS', '30')), elevated=True, timeout=int(os.environ.get('FLASH_TIMEOUT', '300')))
    else:
        sys.exit(__doc__)


if __name__ == '__main__':
    main()
