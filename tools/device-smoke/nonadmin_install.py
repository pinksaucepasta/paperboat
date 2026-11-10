#!/usr/bin/env python3
"""Verify a real administrator refusal: on Unix as an existing non-admin account, on the Windows device
(PB_SMOKE_WINDOWS_PB) as a throwaway standard user with a non-ASCII name that the check creates and removes."""
import argparse
import json
import os
from pathlib import Path
import pwd
import shutil
import subprocess
import secrets
import tempfile

import winhost

# Runs `pb install --json` as a new standard user (a scheduled task with that user's password: the only logon a
# standard user gets without a desktop), then removes the user, its profile and the task. Prints JSON:
# rc, out (base64 of pb's output), new (Paperboat per-user slots or services that appeared).
AS_STANDARD_USER = r"""
$u=$req.user;$pw=ConvertTo-SecureString $req.password -AsPlainText -Force
$slots={@(Get-ChildItem -LiteralPath (Join-Path $env:ProgramFiles 'Paperboat\users'),(Join-Path $env:ProgramData 'Paperboat\users') -ErrorAction SilentlyContinue|ForEach-Object{$_.FullName})+@(Get-Service Paperboat* -ErrorAction SilentlyContinue|ForEach-Object{$_.Name})}
$before=& $slots;$d=Join-Path $env:PUBLIC ('pb-refusal-'+[guid]::NewGuid().ToString('N').Substring(0,12));$t='pb-refusal-'+$u
# A password logon task needs the batch-logon right, which a fresh standard user lacks (granted here, revoked below).
$lsaRights=@'
using System;
using System.Runtime.InteropServices;
public class LsaRights {
  [StructLayout(LayoutKind.Sequential)] public struct U { public ushort Length; public ushort MaximumLength; public IntPtr Buffer; }
  [StructLayout(LayoutKind.Sequential)] public struct A { public int Length; public IntPtr RootDirectory; public IntPtr ObjectName; public uint Attributes; public IntPtr SecurityDescriptor; public IntPtr SecurityQualityOfService; }
  [DllImport("advapi32.dll")] static extern uint LsaOpenPolicy(ref U n, ref A a, int acc, out IntPtr h);
  [DllImport("advapi32.dll")] static extern uint LsaAddAccountRights(IntPtr h, byte[] sid, U[] r, int c);
  [DllImport("advapi32.dll")] static extern uint LsaRemoveAccountRights(IntPtr h, byte[] sid, bool all, U[] r, int c);
  [DllImport("advapi32.dll")] static extern uint LsaClose(IntPtr h);
  static U S(string s){ var u=new U(); u.Buffer=Marshal.StringToHGlobalUni(s); u.Length=(ushort)(s.Length*2); u.MaximumLength=(ushort)((s.Length+1)*2); return u; }
  static IntPtr Open(){ var n=new U(); var a=new A(); a.Length=Marshal.SizeOf(typeof(A)); IntPtr h; if(LsaOpenPolicy(ref n, ref a, 0x10|0x800, out h)!=0) throw new Exception("LsaOpenPolicy"); return h; }
  public static void Grant(byte[] sid,string r){ IntPtr h=Open(); try{ if(LsaAddAccountRights(h,sid,new U[]{S(r)},1)!=0) throw new Exception("grant"); } finally{ LsaClose(h); } }
  public static void Revoke(byte[] sid,string r){ IntPtr h=Open(); try{ if(LsaRemoveAccountRights(h,sid,false,new U[]{S(r)},1)!=0) throw new Exception("revoke"); } finally{ LsaClose(h); } }
}
'@
Add-Type -TypeDefinition $lsaRights -Language CSharp
$account=New-LocalUser -Name $u -Password $pw -PasswordNeverExpires
$sidBytes=New-Object byte[] $account.SID.BinaryLength;$account.SID.GetBinaryForm($sidBytes,0)
[LsaRights]::Grant($sidBytes,'SeBatchLogonRight')
try{
  [void](New-Item -ItemType Directory -Path $d);Copy-Item -LiteralPath $req.exe -Destination (Join-Path $d 'pb.exe')
  & icacls.exe $d /grant "${u}:(OI)(CI)M" | Out-Null
  $line='/c ""'+(Join-Path $d 'pb.exe')+'" install --json > "'+(Join-Path $d 'out')+'" 2>&1 & call echo %^errorlevel% > "'+(Join-Path $d 'rc')+'""'
  [void](Register-ScheduledTask -TaskName $t -Action (New-ScheduledTaskAction -Execute 'cmd.exe' -Argument $line) -User $u -Password $req.password -RunLevel Limited)
  Start-ScheduledTask -TaskName $t
  for($i=0;$i -lt 90 -and -not (Test-Path -LiteralPath (Join-Path $d 'rc'));$i++){Start-Sleep 1}
  $rc=if(Test-Path -LiteralPath (Join-Path $d 'rc')){[int]((Get-Content -LiteralPath (Join-Path $d 'rc') -Raw).Trim())}else{-1}
  $out=if(Test-Path -LiteralPath (Join-Path $d 'out')){$fs=[IO.File]::Open((Join-Path $d 'out'),'Open','Read','ReadWrite');try{$m=New-Object IO.MemoryStream;$fs.CopyTo($m);[Convert]::ToBase64String($m.ToArray())}finally{$fs.Dispose()}}else{''}
  $new=@(& $slots|Where-Object{$before -notcontains $_})
  ConvertTo-Json -Compress -InputObject @{rc=$rc;out=$out;new=$new}
}finally{
  Stop-ScheduledTask -TaskName $t -ErrorAction SilentlyContinue
  Unregister-ScheduledTask -TaskName $t -Confirm:$false -ErrorAction SilentlyContinue
  Get-Process -IncludeUserName -ErrorAction SilentlyContinue|Where-Object{$_.UserName -like "*\$u"}|Stop-Process -Force -ErrorAction SilentlyContinue # a child that outlives pb keeps the profile loaded
  try{[LsaRights]::Revoke($sidBytes,'SeBatchLogonRight')}catch{}
  $sid=$account.SID.Value
  for($i=0;$i -lt 5;$i++){Get-CimInstance Win32_UserProfile -Filter "SID='$sid'" -ErrorAction SilentlyContinue|Remove-CimInstance -ErrorAction SilentlyContinue;if(-not (Get-CimInstance Win32_UserProfile -Filter "SID='$sid'" -ErrorAction SilentlyContinue)){break};Start-Sleep 3}
  Remove-LocalUser -Name $u -ErrorAction SilentlyContinue
  Remove-Item -LiteralPath $d -Recurse -Force -ErrorAction SilentlyContinue
}
"""


def check_windows(device):
    import base64
    user = 'pbλ船' + secrets.token_hex(3)
    answer = json.loads(device.ps('install as a standard user', AS_STANDARD_USER, timeout=240, user=user,
                                  password='Pb-' + secrets.token_urlsafe(18) + '1!', exe=device.pb_exe)[1].splitlines()[-1])
    try:
        receipt = json.loads(base64.b64decode(answer['out']).decode(errors='replace'))
    except ValueError:
        raise RuntimeError('non-admin install did not return a JSON refusal (exit %s)' % answer['rc']) from None
    message = receipt.get('error', {}).get('message', '')
    if answer['rc'] == 0 or receipt.get('ok') is True or not all(
            phrase in message.lower() for phrase in ('administrator', 'pb install')):
        raise RuntimeError('non-admin refusal lost its installation step or recovery action')
    if answer['new']:
        raise RuntimeError('refused installation created a system slot or command')
    print(json.dumps({'check': 'nonadmin-install', 'status': 'PASS', 'specific_refusal': True, 'no_installation': True,
                      'non_ascii_user': True}))


def check(user, binary):
    account = pwd.getpwnam(user)
    if os.geteuid() != 0 or account.pw_uid == 0:
        raise RuntimeError('root must select an existing non-root test account')
    platform = os.uname().sysname
    base = ('/usr/local/libexec/paperboat' if platform == 'Linux' else
            '/Library/PrivilegedHelperTools/Paperboat')
    slot = Path(base) / 'users' / ('u' + str(account.pw_uid))
    if slot.exists():
        raise RuntimeError('test account already has an installation; refusing to touch it')
    prefix = ['sudo', '-u', user, '--']
    permission = subprocess.run(prefix + ['sudo', '-n', 'true'],
                                stdin=subprocess.DEVNULL, capture_output=True, timeout=10)
    if permission.returncode == 0:
        raise RuntimeError('selected account has administrator rights')
    # macOS root's TMPDIR is a private 0700 tree the test account cannot traverse.
    with tempfile.TemporaryDirectory(prefix='pb-refusal-', dir='/tmp' if platform == 'Darwin' else None) as directory:
        root = Path(directory)
        executable = root / 'pb'
        shutil.copyfile(binary, executable)
        executable.chmod(0o700)
        for path in (root, executable):
            os.chown(path, account.pw_uid, account.pw_gid)
        result = subprocess.run(prefix + ['env', 'HOME=' + directory,
                                'XDG_STATE_HOME=' + str(root / 'state'),
                                'XDG_RUNTIME_DIR=' + directory,
                                str(executable), 'install', '--json',
                                '--install-dir', str(root / 'bin')],
                                stdin=subprocess.DEVNULL, capture_output=True,
                                text=True, timeout=45)
        try:
            receipt = json.loads(result.stdout)
        except ValueError:
            raise RuntimeError('non-admin install did not return a JSON refusal') from None
        message = receipt.get('error', {}).get('message', '')
        if result.returncode == 0 or receipt.get('ok') is True or not all(
                phrase in message for phrase in ('supplied Paperboat executable',
                                                  'administrator access', 'pb install')):
            raise RuntimeError('non-admin refusal lost its installation step or recovery action')
        if slot.exists() or (root / 'bin/pb').exists():
            raise RuntimeError('refused installation created a system slot or command')
    print(json.dumps({'check': 'nonadmin-install', 'status': 'PASS',
                      'specific_refusal': True, 'no_installation': True}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--user', help='Unix: an existing non-admin account')
    parser.add_argument('--binary', help='Unix: the pb build to install as that account')
    args = parser.parse_args()
    if winhost.device():
        return check_windows(winhost.device())
    if not args.user or not args.binary:
        parser.error('--user and --binary are required')
    check(args.user, args.binary)


if __name__ == '__main__':
    main()
