#!/usr/bin/env python3
"""windows_console_flash.py CASE [EXE ARGS...]: no Paperboat background work may flash a console on the Windows desktop.

CASE runonce  the uninstall RunOnce entry (conhost --headless cmd /c rd) run as Explorer would at sign-in
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


def run_in_session(device, name, exe, argument_line, seconds, control=False, elevated=False, timeout=0):
    work = device.env('TEMP') + '\\pbflash-' + uuid.uuid4().hex[:8]
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
    device.ps('unregister', "schtasks /Delete /TN $req.t /F | Out-Null", t=TASK, ok=(0, 1))
    if not device.exists(out):
        log = device.read_bytes(work + '\\log.txt').decode(errors='replace')[-400:] if device.exists(work + '\\log.txt') else 'no log'
        sys.exit('FAIL %s: the desktop detector produced no result (is a user signed in on the device?): %s' % (name, log))
    result = json.loads(device.read_bytes(out).decode('utf-8-sig'))
    if device.exists(work + '\\child.txt'):
        result['output'] = device.read_bytes(work + '\\child.txt').decode(errors='replace')[-1500:]
    device.ps('cleanup', 'Remove-Item -Recurse -Force $req.d', d=work)
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
    elif case == 'exec' and len(sys.argv) > 2:  # EXE ARGS...: an administrator's task (HIGHEST: no UAC prompt on the user's desktop)
        check(device, 'exec ' + sys.argv[2].rsplit('\\', 1)[-1] + ' ' + (sys.argv[3] if len(sys.argv) > 3 else ''), sys.argv[2],
              ' '.join(sys.argv[3:]), int(os.environ.get('FLASH_SECONDS', '30')), elevated=True, timeout=int(os.environ.get('FLASH_TIMEOUT', '300')))
    else:
        sys.exit(__doc__)


if __name__ == '__main__':
    main()
