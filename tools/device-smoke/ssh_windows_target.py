#!/usr/bin/env python3
"""The Windows target of the managed-SSH checks: what ssh_mac_rows.py and ssh_trust_host.sh do on the target machine,
for a paired Windows machine reached with winhost (SMOKE_WINDOWS_SSH, PB_SMOKE_WINDOWS_PB; the login is an administrator's).

The managed sshd is Paperboat's own (services PaperboatSshd-*, loopback only): its sshd_config names the port, the
setup user's authorized keys files (Paperboat's and the stock one) and the host key, so everything here is read from the config of the sshd that is running.
Every change is undone by a process on the machine itself after a fixed time, whatever happens to the controller.

  ssh_windows_target.py rotate-host-key    a new ed25519 host key for the managed sshd (F5-04, ROTATE_CMD of ssh_trust_host.sh)
"""
import base64
import json
import re
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
$effective=& ($p[0].ExecutablePath) -T -f $c -C ('user='+$env:USERNAME+',host=example.test,addr=127.0.0.1')
if($LASTEXITCODE -ne 0){throw 'could not resolve the managed sshd configuration for its setup user'}
$v=@{};foreach($l in $effective){$k,$r=$l.Trim() -split '\s+',2;if($k -and $r){if($k -eq 'authorizedkeysfile'){$r=$r -split '\s+'};$v[$k]=@($v[$k])+@($r|ForEach-Object{$_.Trim('"')})}}
$managed=@($v['AuthorizedKeysFile']|Where-Object{$_ -match '^[A-Za-z]:[\\/]'})
# A domain setup account is matched as domain\user. The same config with the setup user renamed to pbsmoke\<user>
# must give Paperboat's file to exactly that name, not to the name with the backslash dropped. The setup user's
# administrators block is left out: its Group criterion would look up an account that does not exist here.
$domain='unset'
if($managed.Count -eq 1){
$u=([IO.File]::ReadAllLines($c)|Where-Object{$_ -match '^Match User "([^"]+)"$'}|ForEach-Object{$Matches[1]}|Select-Object -First 1)
$keep=$true;$lines=foreach($l in [IO.File]::ReadAllLines($c)){if($l -match '^Match '){$keep=$l -notmatch '^Match User .* Group administrators$'};if($keep){$l}}
$t=Join-Path $env:TEMP ('pbsmoke-'+[guid]::NewGuid().ToString('N')+'.conf')
try{[IO.File]::WriteAllLines($t,($lines|ForEach-Object{$_.Replace('Match User "'+$u+'"','Match User "pbsmoke\'+$u+'"')}))
$grant=foreach($n in @('pbsmoke\'+$u),('pbsmoke'+$u)){@(& ($p[0].ExecutablePath) -T -f $t -C ('user='+$n+',host=example.test,addr=127.0.0.1')|Where-Object{$_ -match '^authorizedkeysfile ' -and $_.Contains($managed[0])}).Count}
$domain=if($grant[0] -eq 1 -and $grant[1] -eq 0){'exact'}else{'qualified='+$grant[0]+' unqualified='+$grant[1]}}finally{Remove-Item -Force $t -ErrorAction SilentlyContinue}}
@{service=$s[0].Name;port=[int]@($v['Port'])[0];authorized_keys=@($v['AuthorizedKeysFile']);host_key=@($v['HostKey'])[0];domain_scope=$domain}|ConvertTo-Json -Compress
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
OTHER_USER = r"""
$u=New-LocalUser -Name $req.name -Password (ConvertTo-SecureString $req.password -AsPlainText -Force) -AccountNeverExpires -PasswordNeverExpires
try{
$users=New-Object Security.Principal.SecurityIdentifier 'S-1-5-32-545'
if(-not @(Get-LocalGroupMember -SID $users|Where-Object{$_.SID -eq $u.SID}).Count){Add-LocalGroupMember -SID $users -Member $u}
Add-Type -Namespace PbSmoke -Name Profile -MemberDefinition '[DllImport("userenv.dll", CharSet=CharSet.Unicode)] public static extern int CreateProfile(string sid,string user,System.Text.StringBuilder path,uint size);'
# 260 (MAX_PATH): a 32768-character buffer fails with RPC_X_BAD_STUB_DATA on Windows Server 2025
$b=New-Object Text.StringBuilder 260
$hr=[PbSmoke.Profile]::CreateProfile($u.SID.Value,$req.name,$b,$b.Capacity)
if($hr -ne 0){throw 'could not create the throwaway SSH user profile'}
$dir=Join-Path $b.ToString() '.ssh';[void](New-Item -ItemType Directory -Path $dir)
$key=Join-Path $dir 'authorized_keys';[IO.File]::WriteAllText($key,$req.key+"`n",[Text.Encoding]::ASCII)
$acl=New-Object Security.AccessControl.FileSecurity;$acl.SetOwner($u.SID);$acl.SetAccessRuleProtection($true,$false)
foreach($id in @($u.SID.Value,'S-1-5-18','S-1-5-32-544')){
$sid=New-Object Security.Principal.SecurityIdentifier $id
$acl.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule($sid,'FullControl','Allow')))}
Set-Acl -LiteralPath $key -AclObject $acl
$u.SID.Value
}catch{
$primary=$_
try{Get-CimInstance Win32_UserProfile|Where-Object{$_.SID -eq $u.SID.Value}|Remove-CimInstance -ErrorAction Stop}catch{Write-Warning 'could not remove the failed SSH fixture profile'}
try{Remove-LocalUser -SID $u.SID -ErrorAction Stop}catch{Write-Warning 'could not remove the failed SSH fixture account'}
throw $primary
}
"""
REMOVE_OTHER_USER = r"""
$sid=New-Object Security.Principal.SecurityIdentifier $req.sid
Remove-LocalUser -SID $sid -ErrorAction Stop
"""
# A password logon leaves the profile loaded for a while after the session ends: retry, then leave it with a warning
# (the account is already gone) rather than fail the check on Windows' unload timing.
REMOVE_OTHER_PROFILE = r"""
$deadline=(Get-Date).AddSeconds(60);$e=$null
do{$p=Get-CimInstance Win32_UserProfile|Where-Object{$_.SID -eq $req.sid};if(-not $p){return}
try{$p|Remove-CimInstance -ErrorAction Stop;return}catch{$e=$_};Start-Sleep 5}while((Get-Date) -lt $deadline)
Write-Warning ('the throwaway SSH profile is still loaded; left in place: '+$e.Exception.Message)
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

    def create_other_user(self, name, password, key):
        return self.device.ps('create the throwaway SSH account', OTHER_USER,
                              name=name, password=password, key=key)[1].strip()

    def remove_other_user(self, sid):
        self.device.ps('remove the throwaway SSH account', REMOVE_OTHER_USER, sid=sid)
        # End any SSH logon token before asking Windows to unload its profile.
        self.device.service('restart', self.sshd['service'])
        self.device.ps('remove the throwaway SSH profile', REMOVE_OTHER_PROFILE, timeout=120, sid=sid)

    def remote(self):
        """The commands a client runs ON the target through ssh: cmd.exe is the shell there, so no sh, sleep or true."""
        return {'NOP': 'exit 0', 'HOLD_PB': ssh_command("'session-up';Start-Sleep 300"),
                'HOLD_PLAIN': ssh_command("'plain-up';Start-Sleep 300"), 'HOLD_FWD': ssh_command('Start-Sleep 40'),
                'SSHD_PORT': str(self.sshd['port']), 'AGENT_LIST': r'"%ProgramFiles%\OpenSSH\ssh-add.exe" -l',
                'SHOW_DISPLAY': ssh_command("'display=<'+$env:DISPLAY+'>'")}

    def keys_file(self):
        """Paperboat's key file: the one absolute AuthorizedKeysFile for the setup user (the others are the stock
        Windows files that user keeps: .ssh/authorized_keys or __PROGRAMDATA__ administrators_authorized_keys),
        or None when the config names more or fewer."""
        files = [f for f in self.sshd['authorized_keys'] if re.match(r'[A-Za-z]:[\\/]', f)]
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
