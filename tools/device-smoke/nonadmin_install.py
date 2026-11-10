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
# rc, out (base64 of pb's output), new (Paperboat per-user slots or services that appeared), bin (pb.exe installed).
AS_STANDARD_USER = r"""
$u=$req.user;$pw=ConvertTo-SecureString $req.password -AsPlainText -Force
$slots={@(Get-ChildItem -LiteralPath (Join-Path $env:ProgramFiles 'Paperboat\users'),(Join-Path $env:ProgramData 'Paperboat\users') -ErrorAction SilentlyContinue|ForEach-Object{$_.FullName})+@(Get-Service Paperboat* -ErrorAction SilentlyContinue|ForEach-Object{$_.Name})}
$before=& $slots;$d=Join-Path $env:PUBLIC ('pb-refusal-'+[guid]::NewGuid().ToString('N').Substring(0,12));$t='pb-refusal-'+$u
[void](New-LocalUser -Name $u -Password $pw -PasswordNeverExpires)
try{
  [void](New-Item -ItemType Directory -Path $d);Copy-Item -LiteralPath $req.exe -Destination (Join-Path $d 'pb.exe')
  & icacls.exe $d /grant "${u}:(OI)(CI)M" | Out-Null
  $line='/c ""'+(Join-Path $d 'pb.exe')+'" install --json --install-dir "'+(Join-Path $d 'bin')+'" > "'+(Join-Path $d 'out')+'" 2>&1 & call echo %^errorlevel% > "'+(Join-Path $d 'rc')+'""'
  [void](Register-ScheduledTask -TaskName $t -Action (New-ScheduledTaskAction -Execute 'cmd.exe' -Argument $line) -User $u -Password $req.password -RunLevel Limited)
  Start-ScheduledTask -TaskName $t
  for($i=0;$i -lt 90 -and -not (Test-Path -LiteralPath (Join-Path $d 'rc'));$i++){Start-Sleep 1}
  $rc=if(Test-Path -LiteralPath (Join-Path $d 'rc')){[int]((Get-Content -LiteralPath (Join-Path $d 'rc') -Raw).Trim())}else{-1}
  $out=if(Test-Path -LiteralPath (Join-Path $d 'out')){[Convert]::ToBase64String([IO.File]::ReadAllBytes((Join-Path $d 'out')))}else{''}
  $new=@(& $slots|Where-Object{$before -notcontains $_})
  ConvertTo-Json -Compress -InputObject @{rc=$rc;out=$out;new=$new;bin=(Test-Path -LiteralPath (Join-Path $d 'bin\pb.exe'))}
}finally{
  Unregister-ScheduledTask -TaskName $t -Confirm:$false -ErrorAction SilentlyContinue
  $sid=(Get-LocalUser -Name $u).SID.Value
  Get-CimInstance Win32_UserProfile -Filter "SID='$sid'" -ErrorAction SilentlyContinue|Remove-CimInstance -ErrorAction SilentlyContinue
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
    if answer['new'] or answer['bin']:
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
