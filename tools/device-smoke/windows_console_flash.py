#!/usr/bin/env python3
"""windows_console_flash.py CASE [EXE ARGS...]: no Paperboat background work may flash a console on the Windows desktop.

CASE runonce  the uninstall RunOnce entry (conhost --headless cmd /c rd) run as Explorer would at sign-in
CASE exec     EXE ARGS... (e.g. pb.exe install ..., pb.exe update ..., pb.exe pair ...) started with no console
Each case first runs a control (plain cmd.exe with no console must open a visible window), so a blind detector fails.
SSH runs in session 0 and cannot see the desktop, so the detector (windows_console_flash.ps1) runs through an
interactive scheduled task of the signed-in user (victus has one; runners do not). Needs VICTUS_PASS, SMOKE_WINDOWS_SSH.
"""
import json
import sys
import time
import uuid

import winhost

TASK = 'pbsmoke-console-flash'


def run_in_session(device, name, exe, argument_line, seconds, control=False):
    work = device.env('TEMP') + '\\pbflash-' + uuid.uuid4().hex[:8]
    device.mkdir(work)
    script, out, done = work + '\\detect.ps1', work + '\\out.json', work + '\\done.txt'
    device.write_bytes(script, open(__file__.replace('.py', '.ps1'), 'rb').read())
    arguments = ['-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', script, '-Exe', exe, '-ArgumentLine', argument_line,
                 '-Seconds', str(seconds), '-Out', out]
    if control:
        arguments.append('-Control')
    line = 'powershell.exe ' + ' '.join('"%s"' % a.replace('"', '\\"') for a in arguments)
    device.ps('register ' + name, "$u=[Security.Principal.WindowsIdentity]::GetCurrent().Name;"
              "schtasks /Create /TN $req.t /SC ONCE /ST 00:00 /RU $u /IT /RL LIMITED /F /TR $req.c | Out-Null;"
              "if($LASTEXITCODE){throw 'schtasks /Create failed'};schtasks /Run /TN $req.t | Out-Null", t=TASK, c=line)
    deadline = time.time() + seconds * 3 + 60
    while time.time() < deadline and not device.exists(out):
        time.sleep(2)
    device.ps('unregister', "schtasks /Delete /TN $req.t /F | Out-Null", t=TASK, ok=(0, 1))
    if not device.exists(out):
        sys.exit('FAIL %s: the desktop detector produced no result (is a user signed in on the device?)' % name)
    result = json.loads(device.read_bytes(out).decode('utf-8-sig'))
    device.ps('cleanup', 'Remove-Item -Recurse -Force $req.d', d=work)
    return result


def check(device, name, exe, argument_line, seconds=30):
    control = run_in_session(device, name + ' control', 'cmd.exe', '/c ping -n 3 127.0.0.1', 5, control=True)
    if not control['flashes']:
        sys.exit('FAIL %s: the detector saw no window for the control; the check is blind' % name)
    result = run_in_session(device, name, exe, argument_line, seconds)
    if result['flashes']:
        sys.exit('FAIL %s: windows opened by %s: %s' % (name, result['command'], json.dumps(result['flashes'])))
    print('PASS %s: no visible console window for %ss after %s (exit %s)' % (name, seconds, result['command'], result['exit']))


def main():
    device = winhost.device()
    case = sys.argv[1] if len(sys.argv) > 1 else sys.exit(__doc__)
    if case == 'runonce':
        system = device.env('SystemRoot') + '\\System32'
        helper = device.env('TEMP') + '\\Paperboat Uninstall\\' + uuid.uuid4().hex
        device.mkdir(helper)
        check(device, 'runonce', system + '\\conhost.exe', '--headless %s\\cmd.exe /d /c rd /s /q "%s"' % (system, helper), 10)
        if device.exists(helper):
            sys.exit('FAIL runonce: the helper directory was not removed')
    elif case == 'exec' and len(sys.argv) > 2:
        check(device, 'exec ' + sys.argv[2], sys.argv[2], ' '.join(sys.argv[3:]))
    else:
        sys.exit(__doc__)


if __name__ == '__main__':
    main()
