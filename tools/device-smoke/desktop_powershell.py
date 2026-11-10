#!/usr/bin/env python3
"""desktop_powershell.py LEVEL COMMAND...: run COMMAND in an interactive PowerShell window of the signed-in desktop
session (what a user typing it would do), LEVEL = LIMITED or HIGHEST token; prints the window's output and exit code.
Needs SMOKE_WINDOWS_SSH to a device with an Active console session (a hosted runner); no secrets are printed."""
import os
import sys
import time
import uuid

import winhost


def main():
    level, command = sys.argv[1], ' '.join(sys.argv[2:])
    device = winhost.WindowsDevice()
    work = device.env('TEMP') + '\\pbdesk-' + uuid.uuid4().hex[:8]
    device.mkdir(work)
    log, script = work + '\\out.txt', work + '\\run.ps1'
    if command.startswith('@'):  # PS script from a local file
        command = 'PS:' + open(command[1:]).read()
    if command.startswith('PS:'):  # a PowerShell script body as is; its last line should print the result
        body = "& { %s } *>&1 | Tee-Object -FilePath '%s'\r\n'exit 0' | Out-File -Append -Encoding ascii '%s'\r\n" % (command[3:], log, log)
    else:
        body = ("& cmd /c \"%s 2>&1\" | Tee-Object -FilePath '%s'\r\n'exit ' + $LASTEXITCODE | Out-File -Append -Encoding ascii '%s'\r\n"
                                % (command.replace('"', '""'), log, log))
    device.write_bytes(script, body.encode())
    task = 'powershell.exe -NoProfile -ExecutionPolicy Bypass -File ' + script
    if os.environ.get('VIA_EXPLORER'):  # explorer starts it: outside the task's job object, like a user opening a terminal
        device.write_bytes(work + '\\launch.cmd', ('@powershell.exe -NoProfile -ExecutionPolicy Bypass -File ' + script + '\r\n').encode())
        task = 'C:\\Windows\\explorer.exe ' + work + '\\launch.cmd'
    device.ps('register', "$u=[Security.Principal.WindowsIdentity]::GetCurrent().Name;"
              "schtasks /Create /TN pbdesk /SC ONCE /ST 00:00 /RU $u /IT /RL $req.l /F /TR $req.c | Out-Null;"
              "if($LASTEXITCODE){throw 'schtasks /Create failed'};schtasks /Run /TN pbdesk | Out-Null", l=level, c=task)
    deadline, text = time.time() + 600, ''
    while time.time() < deadline:
        time.sleep(5)
        if device.exists(log):
            text = device.read_bytes(log).decode(errors='replace')
            if 'exit ' in text.splitlines()[-1:][0:1] [0] if text.strip() else False:
                break
    device.ps('unregister', "schtasks /Delete /TN pbdesk /F | Out-Null; Remove-Item -Recurse -Force $req.d -ErrorAction SilentlyContinue", d=work, ok=(0, 1))
    print(text.strip()[-1500:] or 'no output')


if __name__ == '__main__':
    main()
