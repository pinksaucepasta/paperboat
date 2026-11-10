#!/usr/bin/env python3
"""The Windows target of the managed-SSH checks: what ssh_mac_rows.py and ssh_trust_host.sh do on the target machine,
for a paired Windows machine reached with winhost (SMOKE_WINDOWS_SSH, PB_SMOKE_WINDOWS_PB; the login is an administrator's).

The managed sshd is Paperboat's own (services PaperboatSshd-*, loopback only): its sshd_config names the port, the one
authorized keys file and the host key, so everything here is read from the config of the sshd that is running.
Every change is undone by a process on the machine itself after a fixed time, whatever happens to the controller.

  ssh_windows_target.py rotate-host-key    a new ed25519 host key for the managed sshd (F5-04, ROTATE_CMD of ssh_trust_host.sh)
"""
import base64
import json
import sys
import time

import winhost
from windows_target import ssh_command

# The one sshd Paperboat supervises (`sshd.exe -D -e -f <config>`), its service (the parent pb.exe) and its config.
SSHD = r"""
$p=@(Get-CimInstance Win32_Process -Filter "Name='sshd.exe'"|Where-Object{$_.CommandLine -match ' -D -e -f "?([^"]+sshd_config)"?\s*$'})
if($p.Count -ne 1){throw "expected one Paperboat-managed sshd, found $($p.Count)"}
[void]($p[0].CommandLine -match ' -f "?([^"]+sshd_config)"?\s*$');$c=$Matches[1]
$s=@(Get-CimInstance Win32_Service|Where-Object{$_.ProcessId -eq $p[0].ParentProcessId -and $_.Name -like 'PaperboatSshd-*'})
if($s.Count -ne 1){throw 'the managed sshd is not run by a PaperboatSshd service'}
$v=@{};foreach($l in [IO.File]::ReadAllLines($c)){$k,$r=$l.Trim() -split '\s+',2;if($k -and $k[0] -ne '#'){$v[$k]=@($v[$k])+@($r.Trim('"'))|Where-Object{$_}}}
@{service=$s[0].Name;port=[int]@($v['Port'])[0];authorized_keys=@($v['AuthorizedKeysFile']);host_key=@($v['HostKey'])[0]}|ConvertTo-Json -Compress
"""
COUNT = "@([IO.File]::ReadAllLines($req.path)|Where-Object{$_.Contains($req.text)}).Count"
# A new key pair written INTO the existing files: they keep the owner and ACL sshd and Paperboat require.
ROTATE = r"""
$d=Join-Path $env:TEMP ('pbsmoke-'+[guid]::NewGuid().ToString('N'));[void](New-Item -ItemType Directory -Path $d)
try{& (Join-Path $env:ProgramFiles 'OpenSSH\ssh-keygen.exe') -q -t ed25519 -N '""' -f (Join-Path $d 'k')
if($LASTEXITCODE -ne 0){throw 'ssh-keygen failed'}
[IO.File]::WriteAllBytes($req.key,[IO.File]::ReadAllBytes((Join-Path $d 'k')))
[IO.File]::WriteAllBytes($req.key+'.pub',[IO.File]::ReadAllBytes((Join-Path $d 'k.pub')))}finally{Remove-Item -Recurse -Force $d}
"""
BLOCK = "[void](New-NetFirewallRule -DisplayName $req.name -Direction Outbound -Action Block -RemoteAddress $req.addresses)"
UNBLOCK = "Remove-NetFirewallRule -DisplayName $req.name -ErrorAction SilentlyContinue"
PIDS = ("@(Get-CimInstance Win32_Service|Where-Object{$_.Name -like 'Paperboat*' -and $_.State -eq 'Running' -and $_.ProcessId}|"
        "ForEach-Object{$_.ProcessId})|ConvertTo-Json -Compress")
NT = ("Add-Type -Namespace Pb -Name Ps -MemberDefinition '[DllImport(\"ntdll.dll\")] public static extern int NtSuspendProcess(IntPtr h); "
      "[DllImport(\"ntdll.dll\")] public static extern int NtResumeProcess(IntPtr h);';")


class Target:
    def __init__(self):
        self.device = winhost.device() or sys.exit('set PB_SMOKE_WINDOWS_PB (and SMOKE_WINDOWS_SSH) to the Windows target')
        self.sshd = json.loads(self.device.ps('managed sshd', SSHD, timeout=180)[1])

    def user(self):
        return self.device.env('USERNAME')

    def remote(self):
        """The commands a client runs ON the target through ssh: cmd.exe is the shell there, so no sh, sleep or true."""
        return {'NOP': 'exit 0', 'HOLD_PB': ssh_command("'session-up';Start-Sleep 300"),
                'HOLD_PLAIN': ssh_command("'plain-up';Start-Sleep 300"), 'HOLD_FWD': ssh_command('Start-Sleep 40'),
                'SSHD_PORT': str(self.sshd['port']), 'AGENT_LIST': r'"%ProgramFiles%\OpenSSH\ssh-add.exe" -l',
                'SHOW_DISPLAY': ssh_command("'display=<'+$env:DISPLAY+'>'")}

    def keys_file(self):
        """The one file the managed sshd reads keys from, or None when its config names more or fewer."""
        files = self.sshd['authorized_keys']
        return files[0] if len(files) == 1 else None

    def listed(self, blob):
        return self.device.ps('read the managed authorized keys', COUNT, path=self.keys_file(), text=blob)[1] == '1'

    def later(self, seconds, script):
        """Run script on the machine after seconds, detached from this session (the timed undo)."""
        encoded = base64.b64encode(('Start-Sleep %d;' % seconds + script).encode('utf-16-le')).decode()
        with self.device.temp_dir('pb-ssh-rows') as directory:
            self.device.start(['powershell', '-NoProfile', '-NonInteractive', '-EncodedCommand', encoded], directory)

    def block(self, addresses, seconds=240):
        """Drop this machine's traffic to addresses until unblock(), or for seconds at most."""
        name = 'pbsmoke-%d' % time.time()
        self.later(seconds, 'Remove-NetFirewallRule -DisplayName %s' % name)
        self.device.ps('block the control plane', BLOCK, name=name, addresses=list(addresses))
        return name

    def unblock(self, name):
        self.device.ps('unblock the control plane', UNBLOCK, name=name)

    def stop_sshd(self, seconds=40):
        """One detached script stops the managed sshd and starts it again after seconds."""
        name = winhost.ps_quote(self.sshd['service'])
        self.later(0, 'Stop-Service -Name %s -Force;Start-Sleep %d;Start-Service -Name %s' % (name, seconds, name))

    def sshd_running(self):
        return any(s['Status'] == 'Running' for s in self.device.services(self.sshd['service']))

    def daemon_pids(self):
        """The processes of the running Paperboat services: what `docker pause` freezes on a Linux target."""
        return [str(pid) for pid in json.loads(self.device.ps('Paperboat service processes', PIDS)[1] or '[]')]

    def pause(self, pids, seconds=50):
        resume = NT + 'foreach($i in %s){[void][Pb.Ps]::NtResumeProcess((Get-Process -Id $i).Handle)}' % ','.join(pids)
        self.later(seconds, resume)
        for pid in pids:
            self.device.suspend(pid)

    def resume(self, pids):
        for pid in pids:
            self.device.resume(pid)

    def rotate_host_key(self):
        hostd = [s['Name'] for s in self.device.services('PaperboatHostd-*') if s['Status'] == 'Running']
        if len(hostd) != 1:
            sys.exit('expected exactly one running host service, found %r' % hostd)
        self.device.ps('rotate the managed host key', ROTATE, timeout=120, key=self.sshd['host_key'])
        for name in (self.sshd['service'], hostd[0]):  # sshd serves the new key; the host runtime observes it
            self.device.service('restart', name)


if __name__ == '__main__':
    if sys.argv[1:] != ['rotate-host-key']:
        sys.exit(__doc__)
    Target().rotate_host_key()
