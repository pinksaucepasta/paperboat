"""Drive a Windows device from the Linux controller: only pb.exe and small PowerShell snippets run there.

The device-smoke checks keep their logic on the controller. Set PB_SMOKE_WINDOWS_PB to the pb.exe path from the
saved `pb install --json` receipt (install_path.py prints it) and pb_path() returns a WindowsPb that
pairing_lifecycle.command_result runs over SSH, so no check needs python3 on the device. VICTUS_PASS (never
printed) and SMOKE_WINDOWS_SSH (default pujan@victus, as smoke.sh uses) pick the login.

Every device call sends a PowerShell script and its JSON payload on stdin behind a one-line -EncodedCommand
bootstrap (the command line stays far below cmd.exe's 8191 characters whatever the payload size). pb runs through
System.Diagnostics.Process with byte-exact stdin/stdout/stderr, an env overlay and a hard timeout that kills the
process tree, and comes back as one JSON envelope, so PowerShell's CLIXML/encoding quirks never touch pb output.
"""
import base64
import contextlib
import functools
import gzip
import hashlib
import json
import os
import pathlib
import random
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import uuid

DEFAULT_TARGET = 'pujan@victus'
MISSING = 3  # exit code of a snippet whose file does not exist
REFUSED = 4  # exit code of EXCHANGE when nothing accepts the connection

# Runs after stdin has set $a to "<base64 script>", "<base64 JSON payload in lines>": the script runs with $req set.
BOOTSTRAP = ("$ErrorActionPreference='Stop';$ProgressPreference='SilentlyContinue';"
             "$s=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($a[0].Trim()));"
             "$req=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String((($a[1..($a.Length-1)]|ForEach-Object{$_.Trim()}) -join '')))|ConvertFrom-Json;"
             "try{Invoke-Expression $s}catch{[Console]::Error.WriteLine($_);exit 1};exit 0")

RUN = r"""
$ErrorActionPreference='Stop'
$i=New-Object Diagnostics.ProcessStartInfo
$i.FileName=$req.exe;$i.Arguments=$req.args;$i.UseShellExecute=$false
$i.RedirectStandardInput=$true;$i.RedirectStandardOutput=$true;$i.RedirectStandardError=$true
if($req.cwd){$i.WorkingDirectory=$req.cwd}
if($req.env){foreach($e in $req.env.PSObject.Properties){$i.EnvironmentVariables[$e.Name]=[string]$e.Value}}
$p=[Diagnostics.Process]::Start($i)
$om=New-Object IO.MemoryStream;$em=New-Object IO.MemoryStream
if($req.delay_ms -gt 0){Start-Sleep -Milliseconds $req.delay_ms}
$ot=$p.StandardOutput.BaseStream.CopyToAsync($om);$et=$p.StandardError.BaseStream.CopyToAsync($em)
try{$b=$(if($req.input_file){[IO.File]::ReadAllBytes($req.input_file)}else{[Convert]::FromBase64String($req.input)});$p.StandardInput.BaseStream.Write($b,0,$b.Length)}catch{}
try{$p.StandardInput.Close()}catch{}
$t=-not $p.WaitForExit([int]$req.timeout_ms)
if($t){& cmd.exe /c "taskkill /T /F /PID $($p.Id) >nul 2>&1";[void]$p.WaitForExit(10000)}
[void]$ot.Wait(5000);[void]$et.Wait(5000)
[Console]::Out.Write('{"rc":'+$(if($t){-1}else{$p.ExitCode})+',"timed_out":'+$(if($t){'true'}else{'false'})+',"out":"')
[Console]::Out.Write([Convert]::ToBase64String($om.ToArray()));[Console]::Out.Write('","err":"')
[Console]::Out.Write([Convert]::ToBase64String($em.ToArray()));[Console]::Out.Write('"}')
"""

READ = ("if(-not(Test-Path -LiteralPath $req.path -PathType Leaf)){exit 3}\n"
        # FileShare ReadWrite: a running process may still have the file open for writing (a job's output).
        "$f=New-Object IO.FileStream($req.path,[IO.FileMode]::Open,[IO.FileAccess]::Read,[IO.FileShare]::ReadWrite)\n"
        "$m=New-Object IO.MemoryStream;$f.CopyTo($m);$f.Close();[Convert]::ToBase64String($m.ToArray())")
WRITE = ("$d=Split-Path -Parent $req.path;if($d){[void](New-Item -ItemType Directory -Force -Path $d)}\n"
         "$f=New-Object IO.FileStream($req.path,$(if($req.append){[IO.FileMode]::Append}else{[IO.FileMode]::Create}))\n"
         "$b=[Convert]::FromBase64String($req.data);$f.Write($b,0,$b.Length);$f.Close()")
# Windows OpenSSH stalls when more than a few KB arrive on a session's stdin (a 40 KB write never answered on victus),
# so anything larger travels in pieces of this many bytes.
CHUNK = 3000
EXISTS = "if(Test-Path -LiteralPath $req.path){'yes'}else{'no'}"
OWNER_ONLY = ('& icacls.exe $req.path /inheritance:r /grant:r "$([Security.Principal.WindowsIdentity]::GetCurrent().Name):(F)" | Out-Null\n'
              "if($LASTEXITCODE -ne 0){throw 'icacls failed'}")
# The enrollment token file ACL pb's pair requires (bootstrap token_file_windows.go): exactly SYSTEM and the current user,
# full control, nothing inherited, owned by the user (OWNER_ONLY's user alone is refused: 1 ACE instead of 2; a file
# created over ssh is owned by Administrators and carries their ACE).
TOKEN_FILE = ('$u=[Security.Principal.WindowsIdentity]::GetCurrent().Name\n'
              '& icacls.exe $req.path /setowner $u | Out-Null;if($LASTEXITCODE -ne 0){throw "icacls setowner failed"}\n'
              '& icacls.exe $req.path /inheritance:r /grant:r "${u}:(F)" "*S-1-5-18:(F)" | Out-Null;if($LASTEXITCODE -ne 0){throw "icacls grant failed"}\n'
              '& icacls.exe $req.path /remove "*S-1-5-32-544" | Out-Null\n'
              "if($LASTEXITCODE -ne 0){throw 'icacls remove failed'}")  # a file made over ssh is owned by Administrators
# pb's own private file ACL (configsync currentPrivateFileDescriptor): protected, full control for SYSTEM,
# Administrators and the user, in that order. pb compares the DACL text, so icacls (which orders entries itself) won't do.
PRIVATE = ("$u=[Security.Principal.WindowsIdentity]::GetCurrent().User.Value\n"
           "$d='D:P(A;;FA;;;SY)(A;;FA;;;BA)';if($u -ne 'S-1-5-18'){$d+=\"(A;;FA;;;$u)\"}\n"
           "$s=New-Object Security.AccessControl.FileSecurity\n"
           "$s.SetSecurityDescriptorSddlForm($d,[Security.AccessControl.AccessControlSections]::Access)\n"
           "Set-Acl -LiteralPath $req.path -AclObject $s")
# Stop, start or restart the one service whose name matches $req.pattern and wait until it is there (systemctl stop/start/restart).
SERVICE = ("$s=@(Get-Service -Name $req.pattern -ErrorAction SilentlyContinue)\n"
           "if($s.Count -ne 1){throw ('expected one service '+$req.pattern+', found '+$s.Count)}\n"
           "if($req.action -ne 'start'){Stop-Service -InputObject $s[0] -Force;$s[0].WaitForStatus('Stopped','00:01:00')}\n"
           "if($req.action -ne 'stop'){Start-Service -InputObject $s[0];$s[0].WaitForStatus('Running','00:01:00')}")
COPY = "Copy-Item -LiteralPath $req.source -Destination $req.target -Force"
UNLINK = "Remove-Item -LiteralPath $req.path -Recurse -Force -ErrorAction SilentlyContinue"
MKDIR = "[void](New-Item -ItemType Directory -Force -Path $req.path)"
SHA256 = ("if(-not(Test-Path -LiteralPath $req.path -PathType Leaf)){exit 3}\n"
          "(Get-FileHash -LiteralPath $req.path -Algorithm SHA256).Hash.ToLower()")
SIZE = "if(-not(Test-Path -LiteralPath $req.path -PathType Leaf)){exit 3}\n(Get-Item -LiteralPath $req.path).Length"
ENV = ("if($req.name -eq 'STATE'){if($env:XDG_STATE_HOME){$env:XDG_STATE_HOME}"
       "else{[Environment]::GetFolderPath('LocalApplicationData')}}"
       "else{[Environment]::GetEnvironmentVariable($req.name)}")
SERVICES = ("@(Get-Service -Name $req.pattern -ErrorAction SilentlyContinue|"
            "Select-Object Name,@{n='Status';e={[string]$_.Status}},@{n='StartType';e={[string]$_.StartType}})|"
            "ConvertTo-Json -Compress")
# The child never inherits the SSH session's pipes (Windows OpenSSH keeps a session open while a child holds them):
# all three standard handles go to files in $req.dir. $IsWindows is undefined (so true) in Windows PowerShell 5.1;
# -WindowStyle does not exist on pwsh for Linux.
START = ("[void](New-Item -ItemType Directory -Force -Path $req.dir)\n"
         "$z=Join-Path $req.dir 'start.in';[IO.File]::WriteAllBytes($z,[byte[]]@())\n"
         # Windows OpenSSH ends every process of the session when it closes (job object), so a process that must
         # outlive this snippet is created by WMI, outside the session. It has no console window and no handles of ours.
         "if($IsWindows -ne $false){$r=Invoke-CimMethod -ClassName Win32_Process -MethodName Create "
         "-Arguments @{CommandLine=$req.cmdline}\n"
         "if($r.ReturnValue -ne 0){throw ('process create failed: '+$r.ReturnValue)};$r.ProcessId;exit 0}\n"
         # pwsh on Linux (unit tests): a plain detached child with its handles in files.
         "$o=@{FilePath=$req.exe;PassThru=$true;RedirectStandardInput=$z;"
         "RedirectStandardOutput=(Join-Path $req.dir 'start.out');RedirectStandardError=(Join-Path $req.dir 'start.err')}\n"
         "if($req.args){$o.ArgumentList=$req.args}\n"
         "(Start-Process @o).Id")
STOP = ("& cmd.exe /c \"taskkill /T /F /PID $($req.pid) >nul 2>&1\"\n"
        "for($i=0;$i -lt 50 -and (Get-Process -Id $req.pid -ErrorAction SilentlyContinue);$i++){Start-Sleep -Milliseconds 100}\n"
        "if(Get-Process -Id $req.pid -ErrorAction SilentlyContinue){exit 1}")
PROCESSES = ("@(Get-CimInstance Win32_Process|Where-Object{$_.CommandLine -match $req.pattern}|"
             "Select-Object ProcessId,Name)|ConvertTo-Json -Compress")
TASKS = ("@(Get-ScheduledTask -TaskPath $req.path -ErrorAction SilentlyContinue|"
         "Select-Object TaskName,@{n='State';e={[string]$_.State}})|ConvertTo-Json -Compress")
SERVICE_PATHS = ("@(Get-CimInstance Win32_Service|Where-Object{$_.Name -like $req.pattern}|"
                 "Select-Object Name,PathName)|ConvertTo-Json -Compress")
# Outgoing UDP except DNS is dropped while the rule exists (the iptables/pf rule of the Unix checks): pb falls back
# to relays. TCP, and with it the controller's SSH, is untouched. A rule on a disabled firewall would do nothing.
UDP_BLOCK = ("if($req.action -eq 'remove'){Remove-NetFirewallRule -DisplayName $req.name -ErrorAction SilentlyContinue;exit 0}\n"
             "if(@(Get-NetFirewallProfile|Where-Object{-not $_.Enabled}).Count){throw 'Windows Firewall is off for a profile: a block rule would not be enforced'}\n"
             "[void](New-NetFirewallRule -DisplayName $req.name -Direction Outbound -Action Block -Protocol UDP -RemotePort @('1-52','54-65535'))")
# Blocks pb.exe's own traffic (every pb.exe under Program Files\\Paperboat: the daemon, the host runtime, the client) to
# $req.addr and, with $req.inbound, from them; the program scope keeps the controller's SSH (sshd, the runner's tunnel
# ssh.exe) alive, which a machine-wide cut would sever. The firewall drops silently: no reset, no link-down.
NET_CUT = ("if($req.action -eq 'remove'){Remove-NetFirewallRule -DisplayName $req.name -ErrorAction SilentlyContinue;exit 0}\n"
           "if(@(Get-NetFirewallProfile|Where-Object{-not $_.Enabled}).Count){throw 'Windows Firewall is off for a profile: a block rule would not be enforced'}\n"
           "$p=@(Get-ChildItem -LiteralPath (Join-Path $env:ProgramFiles 'Paperboat') -Recurse -Filter pb.exe -File|ForEach-Object{$_.FullName})\n"
           "if(-not $p.Count){throw 'no pb.exe under Program Files\\Paperboat'}\n"
           "foreach($d in @('Outbound')+$(if($req.inbound){@('Inbound')}else{@()})){foreach($x in $p){\n"
           "[void](New-NetFirewallRule -DisplayName $req.name -Direction $d -Action Block -Program $x -RemoteAddress @($req.addr))}}")
# Every non-loopback IPv4 and global/link-local/unique-local IPv6 address.
NON_LOOPBACK = ['0.0.0.0-126.255.255.255', '128.0.0.0-255.255.255.255', '2000::/3', 'fe80::/10', 'fc00::/7']
LISTENING = ("try{$c=New-Object Net.Sockets.TcpClient('127.0.0.1',[int]$req.port);$c.Close();'yes'}catch{'no'}")
ALIVE = "if(Get-Process -Id $req.pid -ErrorAction SilentlyContinue){'yes'}else{'no'}"
WHICH = "(Get-Command $req.name -CommandType Application -ErrorAction SilentlyContinue|Select-Object -First 1).Source"
# Connects, sends $req.block $req.repeat times, half-closes, and returns up to 4 KiB of the reply as base64.
EXCHANGE = ("$c=New-Object Net.Sockets.TcpClient\ntry{$c.Connect($req.address,[int]$req.port)}catch{exit 4}\n"
            "$c.SendTimeout=20000;$c.ReceiveTimeout=20000;$s=$c.GetStream();$b=[Convert]::FromBase64String($req.block)\n"
            "for($i=0;$i -lt $req.repeat;$i++){$s.Write($b,0,$b.Length)}\n"
            "$c.Client.Shutdown([Net.Sockets.SocketShutdown]::Send)\n"
            "$m=New-Object IO.MemoryStream;$r=New-Object byte[] 4096\n"
            "while($m.Length -le 4096){$n=$s.Read($r,0,4096);if($n -le 0){break};$m.Write($r,0,$n)}\n"
            "$c.Close();[Convert]::ToBase64String($m.ToArray())")
# Connects, sends $req.block once and returns the first $req.block.Length bytes of the reply as base64, WITHOUT
# half-closing: a full-duplex echo through a proxy that ends the whole connection on a client FIN.
ECHO = ("$c=New-Object Net.Sockets.TcpClient\ntry{$c.Connect($req.address,[int]$req.port)}catch{exit 4}\n"
        "$c.SendTimeout=20000;$c.ReceiveTimeout=20000;$s=$c.GetStream();$b=[Convert]::FromBase64String($req.block)\n"
        "$s.Write($b,0,$b.Length);$m=New-Object IO.MemoryStream;$r=New-Object byte[] 4096\n"
        "while($m.Length -lt $b.Length){$n=$s.Read($r,0,4096);if($n -le 0){break};$m.Write($r,0,$n)}\n"
        "$c.Close();[Convert]::ToBase64String($m.ToArray())")
# How often $req.text occurs in the files of $req.dir matching $req.filter; a file the daemon is writing is shared.
COUNT = ("$n=0;Get-ChildItem -LiteralPath $req.dir -Filter $req.filter -File -Recurse:([bool]$req.recurse) -ErrorAction SilentlyContinue|ForEach-Object{"
         "$f=[IO.File]::Open($_.FullName,'Open','Read','ReadWrite');$t=(New-Object IO.StreamReader($f)).ReadToEnd();$f.Close();"
         "$n+=[regex]::Matches($t,[regex]::Escape($req.text)).Count};$n")
FILES = ("@(Get-ChildItem -LiteralPath $req.dir -Filter $req.filter -File -ErrorAction SilentlyContinue|"
         "ForEach-Object{$_.Name})|ConvertTo-Json -Compress")
# What PowerShell's tab completion offers for $req.line once the completion script in $req.path is loaded, with the
# directory of $req.exe first on PATH (the script calls `pb __complete`). The script is loaded from its text: the
# execution policy of the device may refuse a .ps1 file.
COMPLETE = ("$env:PATH=(Split-Path -Parent $req.exe)+[IO.Path]::PathSeparator+$env:PATH\n"
            ". ([scriptblock]::Create([IO.File]::ReadAllText($req.path)))\n"
            "$ErrorActionPreference='Continue'  # cobra's __complete always prints a directive line to stderr, which Stop would raise\n"
            "$r=TabExpansion2 $req.line $req.line.Length\n"
            "[Console]::Out.Write((@($r.CompletionMatches|ForEach-Object{$_.CompletionText.Trim()}) -join ','))")
# The lines of the files of $req.dir matching $req.filter that match the regex $req.pattern, base64 of UTF-8.
MATCHING_LINES = ("$o=New-Object Collections.Generic.List[string]\n"
                  "Get-ChildItem -LiteralPath $req.dir -Filter $req.filter -File -ErrorAction SilentlyContinue|ForEach-Object{\n"
                  "$f=[IO.File]::Open($_.FullName,'Open','Read','ReadWrite');$r=New-Object IO.StreamReader($f)\n"
                  "while($null -ne ($l=$r.ReadLine())){if($l -match $req.pattern){$o.Add($l)}};$f.Close()}\n"
                  "[Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes(($o -join \"`n\")))")
# Every file under $req.path as [{p: relative path with /, h: sha256}], base64 of UTF-8 JSON: Windows PowerShell
# writes stdout in the OEM code page, which would change a Unicode file name.
TREE = ("$r=(Get-Item -LiteralPath $req.path).FullName;$s=[Security.Cryptography.SHA256]::Create()\n"
        "$l=@(Get-ChildItem -LiteralPath $r -Recurse -File|ForEach-Object{@{p=$_.FullName.Substring($r.Length+1).Replace('\\','/');"
        "h=-join($s.ComputeHash([IO.File]::ReadAllBytes($_.FullName))|ForEach-Object{$_.ToString('x2')})}})\n"
        "[Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes((ConvertTo-Json -Compress -InputObject $l)))")

# Runs inside the detached wrapper: pb with output in files and the exit code in `rc` (Start-Process writes raw bytes).
SPAWN = ("$o=@{FilePath=$req.exe;RedirectStandardInput=$req.inp;RedirectStandardOutput=$req.out;"
         "RedirectStandardError=$req.err;PassThru=$true}\n"
         "if($IsWindows -ne $false){$o.WindowStyle='Hidden'}\n"
         "if($req.args){$o.ArgumentList=$req.args}\n"
         "$p=Start-Process @o\n"
         "$null=$p.Handle\n"  # without the handle, ExitCode is empty after WaitForExit
         "Set-Content -LiteralPath $req.child -Value $p.Id -Encoding ascii\n"
         "$p.WaitForExit()\n"
         "Set-Content -LiteralPath $req.rc -Value $p.ExitCode -Encoding ascii")

RANDOM_FILE = ("$f=[IO.File]::Create($req.path);$h=[Security.Cryptography.SHA256]::Create()\n"
               "$r=[Security.Cryptography.RandomNumberGenerator]::Create();$b=New-Object byte[] 1048576\n"
               "for($i=0;$i -lt $req.mib;$i++){$r.GetBytes($b);[void]$h.TransformBlock($b,0,$b.Length,$null,0);$f.Write($b,0,$b.Length)}\n"
               "$f.Close();[void]$h.TransformFinalBlock($b,0,0);-join($h.Hash|ForEach-Object{$_.ToString('x2')})")
# The block arrives gzipped: a repeating 64 KiB block is a few hundred bytes on stdin instead of ~117 KB.
PATTERN_FILE = ("$z=New-Object IO.MemoryStream(,[Convert]::FromBase64String($req.block));$m=New-Object IO.MemoryStream\n"
                "$g=New-Object IO.Compression.GZipStream($z,[IO.Compression.CompressionMode]::Decompress);$g.CopyTo($m);$g.Close()\n"
                "$b=$m.ToArray();$f=[IO.File]::Create($req.path)\n"
                "for($i=0;$i -lt $req.repeat;$i++){$f.Write($b,0,$b.Length)}\n$f.Close()")
TRUNCATE_FILE = "$f=[IO.File]::Open($req.path,'Create');$f.SetLength([long]$req.size);$f.Close()"
# Windows has no SIGSTOP/SIGCONT; ntdll suspends and resumes every thread of the process.
SUSPEND = ("Add-Type -Namespace Pb -Name Ps -MemberDefinition '[DllImport(\"ntdll.dll\")] public static extern int NtSuspendProcess(IntPtr h); "
           "[DllImport(\"ntdll.dll\")] public static extern int NtResumeProcess(IntPtr h);'\n"
           "$p=Get-Process -Id $req.pid\n"
           "if($req.action -eq 'suspend'){[void][Pb.Ps]::NtSuspendProcess($p.Handle)}else{[void][Pb.Ps]::NtResumeProcess($p.Handle)}")

# A one-connection-at-a-time HTTP origin on the device's loopback; its port goes to $req.port_file.
ORIGIN = r"""
$l=New-Object Net.Sockets.TcpListener([Net.IPAddress]::Loopback,[int]$req.port);$l.Start()
[IO.File]::WriteAllText($req.port_file,[string]$l.LocalEndpoint.Port)
$b=[Text.Encoding]::UTF8.GetBytes($req.body)
$h=[Text.Encoding]::ASCII.GetBytes("HTTP/1.1 200 OK`r`nContent-Type: text/html`r`nContent-Length: $($b.Length)`r`nConnection: close`r`n`r`n")
while($true){$c=$l.AcceptTcpClient();try{$c.ReceiveTimeout=2000;$s=$c.GetStream();$r=New-Object byte[] 8192
[void]$s.Read($r,0,8192);$s.Write($h,0,$h.Length);$s.Write($b,0,$b.Length);$s.Flush()}catch{}finally{$c.Close()}}
"""

# The plaintext of the CLI's credentials (config.KeyringStore on Windows: one DPAPI file per secret, named by the
# sha256 of its reference) as {file name: base64 value}. Layout: 'PBKR' 2 1 sha256(ref), then the DPAPI blob of
# 'PBKI' 2 1 sha256(ref) uint32-length value, with entropy "paperboat/windows-keyring/v2\0machine\0" + sha256(ref).
DPAPI_SECRETS = ("Add-Type -AssemblyName System.Security;$o=@{}\n"
                 "Get-ChildItem -LiteralPath (Join-Path $env:LOCALAPPDATA 'Paperboat\\credentials') -Filter *.dpapi -File -ErrorAction SilentlyContinue|ForEach-Object{\n"
                 "$b=[IO.File]::ReadAllBytes($_.FullName);if($b.Length -le 38 -or [Text.Encoding]::ASCII.GetString($b,0,4) -cne 'PBKR'){return}\n"
                 "$e=[Text.Encoding]::ASCII.GetBytes(\"paperboat/windows-keyring/v2`0machine`0\")+$b[6..37]\n"
                 "$p=[Security.Cryptography.ProtectedData]::Unprotect([byte[]]$b[38..($b.Length-1)],[byte[]]$e,'CurrentUser')\n"
                 "$o[$_.BaseName]=[Convert]::ToBase64String([byte[]]$p[42..($p.Length-1)])}\n"
                 "ConvertTo-Json -Compress -InputObject $o")
# Who besides the current user, SYSTEM and Administrators may access $req.path (SIDs, one per line).
OTHER_ACCESS = ("$me=[Security.Principal.WindowsIdentity]::GetCurrent().User.Value\n"
                "(Get-Acl -LiteralPath $req.path).Access|Where-Object{$_.AccessControlType -eq 'Allow'}|ForEach-Object{"
                "$_.IdentityReference.Translate([Security.Principal.SecurityIdentifier]).Value}|"
                "Where-Object{$_ -notin $me,'S-1-5-18','S-1-5-32-544'}|Sort-Object -Unique")

# Ctrl-C at a chosen moment: starts $req.exe in a console of its own, evaluates the PowerShell expression $req.when
# every 5 ms and, the first time it is true, sends CTRL_C to that console (the only signal a Windows console program
# gets from a user). Prints "<fired 0|1> <exit code, or -1 when it had to be killed>".
# Ctrl-BREAK to the target's own console (spark/ctrlc-delivery.md, winpool-1: Ctrl-C reached Go's pb.exe 0 of 9 times in
# every delivery, Ctrl-BREAK 6 of 6). The target runs in a real console (no Hidden window). The caller attaches to it,
# arms a swallow handler AFTER attaching (armed before, the BREAK kills the caller too: rc 58), sends the event to group
# 0 and detaches at once; its own stdout is unusable after AttachConsole, so the answer is a file: "fired seconds rc pid"
# (rc -1: the target is still running; the controller reaps it from a fresh session, never from this attached one:
# taskkill here tears the console down and the snippet exits 1 with an empty stderr).
INTERRUPT = r'''Add-Type -Namespace Pb -Name Con -MemberDefinition '[DllImport("kernel32.dll")] public static extern bool FreeConsole(); [DllImport("kernel32.dll")] public static extern bool AttachConsole(uint p); [DllImport("kernel32.dll")] public static extern bool GenerateConsoleCtrlEvent(uint e, uint g);'
Add-Type -TypeDefinition @"
using System; using System.Runtime.InteropServices;
public static class Swallow {
  public delegate bool HR(int t);
  [DllImport("kernel32.dll")] public static extern bool SetConsoleCtrlHandler(HR h, bool a);
  public static bool Eat(int t){ return true; }
  public static HR keep;
  public static void Arm(){ keep = new HR(Eat); SetConsoleCtrlHandler(keep, true); }
}
"@
$w=[scriptblock]::Create($req.when);$o=@{FilePath=$req.exe;PassThru=$true}
if($req.args){$o.ArgumentList=$req.args}
if($req.stdout){$o.RedirectStandardOutput=$req.stdout;$o.RedirectStandardError=$req.stderr}
$p=Start-Process @o;$null=$p.Handle;$fired=0;$d=(Get-Date).AddSeconds($req.seconds)
while(-not $p.HasExited -and (Get-Date) -lt $d){if(& $w){
[void][Pb.Con]::FreeConsole();if(-not [Pb.Con]::AttachConsole($p.Id)){break}
[void][Swallow]::Arm()
$fired=[int][Pb.Con]::GenerateConsoleCtrlEvent(1,0);[void][Pb.Con]::FreeConsole();break}
Start-Sleep -Milliseconds 5}
$t1=Get-Date;while(-not $p.HasExited -and ((Get-Date)-$t1).TotalMilliseconds -lt [int]$req.exit_ms){Start-Sleep -Milliseconds 200}
$dt=[math]::Round(((Get-Date)-$t1).TotalSeconds,1);if($p.HasExited){$rc=$p.ExitCode}else{$rc=-1}
Set-Content -LiteralPath $req.result -Value "$fired $dt $rc $($p.Id)" -NoNewline'''

# Snippets that change nothing on the device: safe to send again when a session did not answer.
READ_ONLY = (READ, EXISTS, SHA256, SIZE, ENV, SERVICES, PROCESSES, TASKS, SERVICE_PATHS, LISTENING, ALIVE)


def _b64(value):
    return base64.b64encode(value).decode('ascii')


def ps_quote(value):
    return "'" + str(value).replace("'", "''") + "'"


def _text(value):
    return value.decode(errors='replace') if isinstance(value, (bytes, bytearray)) else value or ''


class DeviceError(RuntimeError):
    """A device snippet failed; the message names the step."""


class WindowsDevice:
    path_flavor = pathlib.PureWindowsPath  # tests that run the snippets in a local pwsh use PurePosixPath

    def __init__(self, target=None, pb_exe=None, password=None):
        self.target = target or os.environ.get('SMOKE_WINDOWS_SSH', DEFAULT_TARGET)
        self.pb_exe = pb_exe
        self.password = os.environ.get('VICTUS_PASS') if password is None else password
        self._environment = {}

    def ssh_prefix(self, *options):
        ssh = ['ssh', '-o', 'ConnectTimeout=15', '-o', 'ServerAliveInterval=30', '-o', 'StrictHostKeyChecking=accept-new',
               *options]
        return ['sshpass', '-e'] + ssh if self.password else ['ssh', '-o', 'BatchMode=yes'] + ssh[1:]

    def ssh_command(self):
        # PowerShell reads its script from stdin and nothing else does. A script given on the command line that reads
        # stdin itself races PowerShell 5.1, which takes a redirected stdin as pipeline input: whoever loses waits for
        # an end-of-input that Windows OpenSSH does not always deliver (every call hung on some hosted runners).
        return self.ssh_prefix(self.target, 'powershell -NoProfile -NonInteractive -OutputFormat Text -Command -')

    def ssh_environment(self):
        return dict(os.environ, SSHPASS=self.password) if self.password else None

    def snippet(self, step, script, timeout=60, **payload):
        """Run script on the device with $req = payload; return (rc, stdout bytes, stderr bytes)."""
        # The payload goes in short lines: Windows PowerShell 5.1 stalls reading one very long line from an SSH stdin
        # (a 36 KB file write never answered on victus).
        body = _b64(json.dumps(payload).encode())
        # A blank line ends each statement PowerShell reads from stdin.
        stdin = ("$a=@'\n" + _b64(script.encode()) + '\n' + '\n'.join(body[i:i + 4000] for i in range(0, len(body), 4000))
                 + "\n'@ -split \"`r?`n\"\n\n" + BOOTSTRAP + '\n\n')
        try:
            done = subprocess.run(self.ssh_command(), input=stdin.encode(), capture_output=True, timeout=timeout,
                                  env=self.ssh_environment())
        except subprocess.TimeoutExpired as error:
            raise DeviceError('%s: the device did not answer within %ss; check that victus is reachable' %
                              (step, timeout)) from error
        # PowerShell reading its script from stdin can print console-mode escapes (ESC[?1h) ahead of the output.
        return done.returncode, re.sub(rb'\x1b\[[0-9;?]*[a-zA-Z]', b'', done.stdout), done.stderr

    def ps(self, step, script, timeout=60, ok=(0,), **payload):
        if script in READ_ONLY:
            # A session that never answers (seen on victus for a tiny read) or whose ssh itself failed (exit 255: a banner
            # timeout on a hosted runner killed a 20 min transfer check while it polled a job's rc file) is tried again;
            # a read changes nothing. Every attempt is bounded.
            for attempt in range(3):
                try:
                    rc, out, err = self.snippet(step, script, min(timeout, 30) if attempt == 0 else timeout, **payload)
                except DeviceError:
                    if attempt == 2:
                        raise
                    continue
                if rc != 255 or attempt == 2:
                    break
        else:
            rc, out, err = self.snippet(step, script, timeout, **payload)
        if rc not in ok:
            raise DeviceError('%s failed on the device (exit %s): %s' % (step, rc, _text(err).strip()[-300:]))
        return rc, _text(out).strip()

    def run(self, argv, input=None, env=None, timeout=90, cwd=None, text=True, read_delay=0):
        """Run argv[0] on the device like subprocess.run(..., capture_output=True, text=text).

        read_delay seconds pass before anything reads its output: a slow consumer, so the child's pipe backs up."""
        data = input.encode() if isinstance(input, str) else (input or b'')
        with contextlib.ExitStack() as stack:
            staged = ''
            if len(data) > CHUNK:  # too much for the session's stdin: the device reads it from a file
                staged = stack.enter_context(self.temp_dir('pb-input')) / 'stdin'
                staged.write_bytes(data)
                data = b''
            rc, out, err = self.snippet('run ' + pathlib.PureWindowsPath(argv[0]).name, RUN, timeout + 30,
                                    exe=argv[0], args=subprocess.list2cmdline(list(argv[1:])), env=env or {},
                                    cwd=cwd or '', input=_b64(data), input_file=str(staged), timeout_ms=int(timeout * 1000),
                                    delay_ms=int(read_delay * 1000))
        if rc:
            raise DeviceError('run %s failed on the device (exit %s): %s' %
                              (pathlib.PureWindowsPath(argv[0]).name, rc, _text(err).strip()[-300:]))
        try:
            envelope = json.loads(_text(out).strip().splitlines()[-1])
            stdout, stderr = base64.b64decode(envelope['out']), base64.b64decode(envelope['err'])
        except (ValueError, IndexError, KeyError):
            raise DeviceError('run %s returned no result envelope' % argv[0]) from None
        if envelope['timed_out']:
            raise subprocess.TimeoutExpired(argv, timeout, output=stdout, stderr=stderr)
        if not text:
            return subprocess.CompletedProcess(argv, envelope['rc'], stdout, stderr)
        return subprocess.CompletedProcess(argv, envelope['rc'], _text(stdout), _text(stderr))

    def read_bytes(self, path):
        rc, out = self.ps('read %s' % path, READ, ok=(0, MISSING), path=str(path))
        if rc == MISSING:
            raise FileNotFoundError(path)
        return base64.b64decode(out)

    def write_bytes(self, path, data):
        for start in range(0, max(len(data), 1), CHUNK):
            self.ps('write %s' % path, WRITE, path=str(path), data=_b64(data[start:start + CHUNK]), append=start > 0)

    def exists(self, path):
        return self.ps('exists %s' % path, EXISTS, path=str(path))[1] == 'yes'

    def unlink(self, path):
        self.ps('remove %s' % path, UNLINK, path=str(path))

    def mkdir(self, path):
        self.ps('mkdir %s' % path, MKDIR, path=str(path))

    def copy(self, source, target):
        self.ps('copy %s' % source, COPY, source=str(source), target=str(target))

    def sha256(self, path):
        rc, out = self.ps('hash %s' % path, SHA256, ok=(0, MISSING), path=str(path))
        if rc == MISSING:
            raise FileNotFoundError(path)
        return out

    def size(self, path):
        rc, out = self.ps('size %s' % path, SIZE, ok=(0, MISSING), path=str(path))
        if rc == MISSING:
            raise FileNotFoundError(path)
        return int(out)

    def env(self, name):
        if name not in self._environment:
            self._environment[name] = self.ps('environment ' + name, ENV, name=name)[1]
        return self._environment[name]

    def count_in_files(self, directory, pattern, text, recurse=False):
        return int(self.ps('count in %s' % directory, COUNT, dir=str(directory), filter=pattern, text=text,
                           recurse=recurse)[1])

    def files(self, directory, pattern='*'):
        """Names of the files in directory that match the wildcard (not recursive); [] when it does not exist."""
        out = self.ps('list %s' % directory, FILES, dir=str(directory), filter=pattern)[1]
        value = json.loads(out) if out else []
        return [value] if isinstance(value, str) else value

    def completions(self, script, line):
        """What PowerShell offers for line after loading the completion script file (a path on the device)."""
        out = self.ps('powershell completion', COMPLETE, timeout=120, exe=self.pb_exe, path=str(script), line=line)[1]
        return [word for word in out.splitlines()[-1].split(',') if word] if out else []

    def cli_secrets(self):
        """{sha256 hex of the secret's reference: value} of every stored CLI credential; kept in memory only."""
        stored = json.loads(self.ps('read the CLI credentials', DPAPI_SECRETS)[1] or '{}')
        return {name: base64.b64decode(value).decode(errors='replace') for name, value in stored.items()}

    def other_access(self, path):
        """SIDs other than the user, SYSTEM and Administrators that the file's ACL lets in (empty: owner-only)."""
        return self.ps('acl %s' % path, OTHER_ACCESS, path=str(path))[1].split()

    def interrupt_when(self, argv, when, seconds=30, exit_seconds=90, stdout=None, stderr=None):
        """Run argv in its own console and send it Ctrl-BREAK (what interrupts a Go program there) the first time the
        PowerShell expression `when` is true (polled every 5 ms for `seconds`); returns (whether it was sent, exit code
        or -1 if the program had to be killed). The answer goes through a file and a target that outlived the wait is
        stopped from here, a fresh session: see INTERRUPT."""
        # said honestly in the check's output: this is not a Ctrl-C key press (CTRL_C_EVENT never reached pb.exe from this harness, 0 of 9; spark/ctrlc-delivery.md)
        print('interrupt via CTRL_BREAK (a console Ctrl-C key is proven by the terminal Ctrl-C check)', file=sys.stderr, flush=True)
        with self.temp_dir('pb-interrupt') as directory:
            result = directory / 'result.txt'
            self.ps('interrupt ' + pathlib.PureWindowsPath(argv[0]).name, INTERRUPT, timeout=seconds + exit_seconds + 60,
                    exe=argv[0], args=subprocess.list2cmdline(list(argv[1:])), when=when, seconds=seconds,
                    exit_ms=exit_seconds * 1000, stdout=str(stdout) if stdout else None,
                    stderr=str(stderr) if stderr else None, result=str(result))
            fired, _, code, pid = result.read_text().split()
        if int(code) == -1:
            self.stop(int(pid))
        return fired == '1', int(code)

    def matching_lines(self, directory, filter_, pattern):
        """The lines of the files of directory matching the glob filter_ that match the regex pattern (grep)."""
        out = self.ps('lines in %s' % directory, MATCHING_LINES, timeout=120, dir=str(directory), filter=filter_, pattern=pattern)[1]
        return base64.b64decode(out).decode('utf-8', 'replace').splitlines()

    def services(self, pattern='Paperboat*'):
        return self._list('services ' + pattern, SERVICES, pattern=pattern)

    def _list(self, step, script, **payload):
        out = self.ps(step, script, **payload)[1]
        value = json.loads(out) if out else []
        return [value] if isinstance(value, dict) else value

    def service(self, action, pattern):
        """`stop`, `start` or `restart` the one service matching the wildcard (an exact name matches itself) and wait for it
        (needs an elevated session)."""
        assert action in ('stop', 'start', 'restart'), action
        self.ps('%s service %s' % (action, pattern), SERVICE, timeout=180, action=action, pattern=pattern)

    def processes(self, pattern):
        """Processes whose command line matches the regex (pgrep -f). The snippet's own command line is only a
        base64 bootstrap, so it never matches."""
        return self._list('processes ' + pattern, PROCESSES, pattern=pattern)

    def scheduled_tasks(self, path):
        return self._list('tasks ' + path, TASKS, path=path)

    def service_paths(self, pattern):
        """Name and command line (PathName) of the services whose name matches the wildcard."""
        return self._list('service paths ' + pattern, SERVICE_PATHS, pattern=pattern)

    def start(self, argv, directory):
        """Start argv[0] hidden and detached, its standard handles in directory/start.*; return its pid."""
        # 180 s: the first CIM call of a session can take over a minute on a busy Windows machine.
        return int(self.ps('start ' + argv[0], START, timeout=180, exe=argv[0], args=subprocess.list2cmdline(list(argv[1:])),
                           cmdline=subprocess.list2cmdline(list(argv)), dir=str(directory))[1])

    def suspend(self, pid):
        self.ps('suspend %s' % pid, SUSPEND, pid=int(pid), action='suspend')

    def resume(self, pid):
        self.ps('resume %s' % pid, SUSPEND, pid=int(pid), action='resume')

    def stop(self, pid):
        self.ps('stop %s' % pid, STOP, pid=int(pid))

    def alive(self, pid):
        return self.ps('alive %s' % pid, ALIVE, pid=int(pid))[1] == 'yes'

    def spawn(self, argv, directory, env=None):
        """Start argv detached with output in directory/{out,err} and its exit code in directory/rc."""
        directory = RemotePath(self, directory)
        args = subprocess.list2cmdline(list(argv[1:]))
        overlay = ''.join('$env:%s=%s;' % (name, ps_quote(value)) for name, value in (env or {}).items())
        wrapper = _b64((overlay + '$req=@{exe=%s;args=%s;inp=%s;out=%s;err=%s;rc=%s;child=%s};' % tuple(map(ps_quote, (
            argv[0], args, directory / 'start.in', directory / 'out', directory / 'err', directory / 'rc',
            directory / 'child'))) + SPAWN
        ).encode('utf-16-le'))
        pid = self.start(['powershell.exe', '-NoProfile', '-NonInteractive', '-EncodedCommand', wrapper], directory)
        return RemoteProcess(self, pid, directory)

    @contextlib.contextmanager
    def origin(self, body, port=0):
        """An HTTP origin on the device's loopback that answers every request with body; yields its port (a given one
        to start again where an earlier origin stopped)."""
        with self.temp_dir('pb-origin') as directory:
            port_file = directory / 'port'
            script = ('$req=@{port_file=%s;body=%s;port=%d};' % (ps_quote(port_file), ps_quote(body), port) + ORIGIN)
            pid = self.start(['powershell.exe', '-NoProfile', '-NonInteractive', '-EncodedCommand',
                              _b64(script.encode('utf-16-le'))], directory)
            try:
                deadline = time.monotonic() + 30
                while not port_file.exists():
                    if time.monotonic() > deadline:
                        raise DeviceError('origin did not start listening within 30s on the device')
                    time.sleep(0.5)
                yield int(port_file.read_text().strip())
            finally:
                self.stop(pid)

    def cut_network(self, seconds, addresses=None, inbound=True):
        """Drop pb.exe's traffic to `addresses` (default: everything but loopback) until release(), or after `seconds`
        when a detached timer on the device removes the rule whatever happens to this controller."""
        name = 'PaperboatSmoke-cut-' + uuid.uuid4().hex[:12]
        self.ps('cut the network', NET_CUT, action='add', name=name, addr=list(addresses or NON_LOOPBACK), inbound=inbound)
        scratch = self.temp_dir('pb-cut')
        try:
            timer = "Start-Sleep %d; Remove-NetFirewallRule -DisplayName '%s' -ErrorAction SilentlyContinue" % (seconds, name)
            self.start(['powershell.exe', '-NoProfile', '-NonInteractive', '-EncodedCommand',
                        _b64(timer.encode('utf-16-le'))], scratch.__enter__())
        except BaseException:
            self.ps('restore the network', NET_CUT, action='remove', name=name)
            scratch.__exit__(None, None, None)
            raise
        return NetCut(self, name, scratch)

    @contextlib.contextmanager
    def udp_blocked(self):
        """Outgoing UDP except DNS dropped on the device for the duration; removed on every exit path."""
        name = 'PaperboatSmoke-udp-' + uuid.uuid4().hex[:12]
        try:
            self.ps('block outgoing UDP', UDP_BLOCK, action='add', name=name)
            yield
        finally:
            self.ps('unblock outgoing UDP', UDP_BLOCK, action='remove', name=name)

    def listening(self, port):
        return self.ps('listening %s' % port, LISTENING, port=int(port))[1] == 'yes'

    @contextlib.contextmanager
    def forward(self, local_port, host='127.0.0.1'):
        """Reach host:local_port, as the controller reaches it, at 127.0.0.1:<port> on the device (ssh -R); yields
        that port.

        For origins that cannot be a PowerShell listener (recording, WebSocket, TLS, raw TCP): the check keeps
        its own Python server here and pb on the device dials the forwarded port."""
        for _ in range(5):
            port = random.randrange(20000, 60000)
            tunnel = subprocess.Popen(self.ssh_prefix('-N', '-o', 'ExitOnForwardFailure=yes', '-R',
                                                      '127.0.0.1:%d:%s:%d' % (port, host, local_port), self.target),
                                      env=self.ssh_environment(), stdin=subprocess.DEVNULL,
                                      stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            try:
                deadline = time.monotonic() + 20
                while tunnel.poll() is None and not self.listening(port):
                    if time.monotonic() > deadline:
                        raise DeviceError('ssh -R to the device did not open port %d within 20s' % port)
                if tunnel.poll() is None:
                    yield port
                    return
            finally:
                if tunnel.poll() is None:
                    tunnel.terminate()
                    tunnel.wait(timeout=10)
        raise DeviceError('could not forward a port to the device: ssh -R failed 5 times (is TCP forwarding allowed?)')

    def home(self):
        return RemotePath(self, self.env('USERPROFILE'))

    def runtime_dir(self):
        """Where `pb pair` keeps machine-registration.json: userpaths.State("Paperboat/runtime")."""
        return RemotePath(self, self.env('STATE')) / 'Paperboat' / 'runtime'

    @contextlib.contextmanager
    def temp_dir(self, prefix='pb-smoke'):
        path = RemotePath(self, self.env('TEMP')) / ('%s-%s' % (prefix, uuid.uuid4().hex[:12]))
        path.mkdir()
        try:
            yield path
        finally:
            path.unlink()


class NetCut:
    """A firewall cut made by WindowsDevice.cut_network; release() lifts it (also when called twice)."""

    def __init__(self, device, name, scratch):
        self.device, self.name, self.scratch = device, name, scratch

    def release(self):
        if self.scratch is None:
            return
        scratch, self.scratch = self.scratch, None
        try:
            self.device.ps('restore the network', NET_CUT, action='remove', name=self.name)
        finally:
            scratch.__exit__(None, None, None)  # also removes the timer's directory; the timer is idempotent

    __enter__ = lambda self: self
    __exit__ = lambda self, *exc: self.release()


class RemoteProcess:
    """A process started by WindowsDevice.spawn: read its output so far, wait for its exit code, kill its tree."""

    def __init__(self, device, pid, directory):
        self.device, self.pid, self.directory = device, pid, directory

    def _read(self, name):
        try:
            return (self.directory / name).read_bytes()
        except FileNotFoundError:
            return b''

    def stdout(self):
        return self._read('out')

    def stderr(self):
        return self._read('err')

    def returncode(self):
        value = self._read('rc').decode().strip()
        return int(value) if value else None

    def wait(self, timeout, interval=2):
        deadline = time.monotonic() + timeout
        while self.returncode() is None:
            if time.monotonic() > deadline:
                raise subprocess.TimeoutExpired(str(self.directory), timeout, output=self.stdout(), stderr=self.stderr())
            time.sleep(interval)
        return self.returncode()

    def child_pid(self):
        """The pid of pb itself (self.pid is the wrapper that waits for it)."""
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            value = self._read('child').decode().strip()
            if value:
                return int(value)
            time.sleep(.5)
        raise DeviceError('the spawned process did not report its pid within 30s')

    def kill(self):
        self.device.stop(self.pid)


class RemotePath:
    """The part of pathlib.Path the checks use, executed on the device."""

    def __init__(self, device, path):
        self.device, self.path = device, device.path_flavor(str(path))

    def __truediv__(self, name):
        return RemotePath(self.device, self.path / name)

    def __str__(self):
        return str(self.path)

    def __eq__(self, other):
        return isinstance(other, RemotePath) and self.path == other.path

    @property
    def name(self):
        return self.path.name

    @property
    def parent(self):
        return RemotePath(self.device, self.path.parent)

    def read_bytes(self):
        return self.device.read_bytes(self)

    def read_text(self):
        return self.read_bytes().decode()

    def write_bytes(self, data):
        self.device.write_bytes(self, data)

    def write_text(self, value):
        self.write_bytes(value.encode())

    def exists(self):
        return self.device.exists(self)

    def mkdir(self, mode=None, parents=True, exist_ok=True):
        self.device.mkdir(self)

    def unlink(self, missing_ok=True):
        self.device.unlink(self)

    def write_pattern(self, block, repeat):
        self.device.ps('write pattern %s' % self, PATTERN_FILE, timeout=300, path=str(self), block=_b64(gzip.compress(block)),
                       repeat=repeat)

    def write_random(self, size):
        """Random bytes of the given size (whole MiB), made on the device; returns their sha256."""
        assert size % (1 << 20) == 0
        return self.device.ps('write random %s' % self, RANDOM_FILE, timeout=600, path=str(self), mib=size >> 20)[1]

    def truncate(self, size):
        self.device.ps('truncate %s' % self, TRUNCATE_FILE, path=str(self), size=size)

    def chmod(self, mode):
        """Owner-only (no group/other bits): drop inherited entries and grant the current user alone, as pb's
        owner-only file checks require on Windows. Other modes change nothing."""
        if mode & 0o077 == 0:
            self.device.ps('owner-only %s' % self, OWNER_ONLY, path=str(self))

    def enrollment_token_file(self):
        """The ACL pb pair requires of an enrollment token file: SYSTEM and the current user only."""
        self.device.ps('token-file %s' % self, TOKEN_FILE, path=str(self))

    def private(self):
        """The ACL pb gives its own private files and requires of a credential reference (a key, known_hosts)."""
        self.device.ps('private %s' % self, PRIVATE, path=str(self))

    def sha256(self):
        return self.device.sha256(self)

    def tree_hashes(self):
        """{relative path with /: sha256} of every file below this directory."""
        listed = json.loads(base64.b64decode(self.device.ps('hash tree %s' % self, TREE, timeout=300, path=str(self))[1]))
        return {item['p']: item['h'] for item in listed}

    def size(self):
        return self.device.size(self)


class WindowsPb:
    """An executable on the device (pb.exe unless exe names another); str() is its path, run_remote() runs it there."""

    def __init__(self, device, exe=None):
        self.device, self.exe = device, exe or device.pb_exe

    def __str__(self):
        return self.exe

    def run_remote(self, arguments, input=None, env=None, timeout=180, text=True, read_delay=0):
        return self.device.run([self.exe, *arguments], input=input, env=env, timeout=timeout, text=text,
                               read_delay=read_delay)


class LocalJob:
    """pb as a local child with output in temp files (the Unix side of winhost.job); SIGINT is Ctrl-C."""

    def __init__(self, argv):
        self.output, self.errors = tempfile.TemporaryFile(), tempfile.TemporaryFile()
        self.process = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=self.output, stderr=self.errors)

    def stdout(self, limit=(1 << 20) + 1):
        return os.pread(self.output.fileno(), limit, 0)

    def stderr(self):
        return os.pread(self.errors.fileno(), 1 << 20, 0)

    def poll(self):
        return self.process.poll()

    def interrupt(self):
        self.process.send_signal(signal.SIGINT)

    def suspend(self):
        self.process.send_signal(signal.SIGSTOP)

    def resume(self):
        self.process.send_signal(signal.SIGCONT)

    def wait(self, timeout):
        return self.process.wait(timeout=timeout)

    def terminate(self):
        """SIGTERM, so pb ends its own children (pb ssh's ssh client); killed after 15 s."""
        self.process.terminate()
        try:
            self.process.wait(timeout=15)
        except subprocess.TimeoutExpired:
            self.kill()

    def kill(self):
        self.process.kill()
        self.process.wait()

    def close(self):
        self.output.close()
        self.errors.close()


class RemoteJob:
    """pb.exe on the Windows device with output read from files there. A hidden Windows process gets no Ctrl-C."""

    def __init__(self, device, argv, env=None):
        self.device, self.scratch = device, device.temp_dir('pb-job')
        self.process = device.spawn(argv, self.scratch.__enter__(), env)

    def stdout(self, limit=(1 << 20) + 1):
        return self.process.stdout()[:limit]

    def stderr(self):
        return self.process.stderr()[:1 << 20]

    def poll(self):
        return self.process.returncode()

    def interrupt(self):
        raise RuntimeError('Ctrl-C cannot be delivered to a hidden Windows process')

    def suspend(self):
        self.device.suspend(self.process.child_pid())

    def resume(self):
        self.device.resume(self.process.child_pid())

    def wait(self, timeout):
        try:
            return self.process.wait(timeout, interval=1)
        except subprocess.TimeoutExpired as error:
            raise subprocess.TimeoutExpired(error.cmd, timeout) from None

    def kill(self):
        self.process.kill()

    terminate = kill  # taskkill /T ends the whole tree

    def close(self):
        self.scratch.__exit__(None, None, None)


def job(binary, arguments):
    """Start `pb <arguments>` in the background: a RemoteJob for the device's pb.exe, else a LocalJob."""
    if hasattr(binary, 'device'):
        return RemoteJob(binary.device, [str(binary), *arguments])
    return LocalJob([str(binary), *arguments])


def terminal_argv(binary, arguments, cwd=None):
    """argv for a local pty harness (terminal_lifecycle.Terminal) to run `pb <arguments>` interactively (cwd: a directory of the device).

    On the device it is `ssh -tt`, so Windows OpenSSH gives pb.exe a ConPTY and the harness drives that console.
    sshpass reads the password from SSHPASS: pass terminal_env(binary) to that child only."""
    if not hasattr(binary, 'device'):
        return [str(binary), *arguments]
    target = binary.device
    command = "& %s %s" % (ps_quote(binary), ' '.join(ps_quote(word) for word in arguments))
    if cwd is not None:  # the device directory pb starts in (pb new opens its terminal there)
        command = "Set-Location -LiteralPath %s; %s" % (ps_quote(cwd), command)
    return target.ssh_prefix('-tt', target.target, 'powershell -NoProfile -NonInteractive -Command ' + subprocess.list2cmdline([command]))


def terminal_env(binary):
    """Extra environment for the terminal_argv child alone; the password never enters this process's environment."""
    target = getattr(binary, 'device', None)
    return {'SSHPASS': target.password} if target is not None and target.password else {}


def run_pb(binary, arguments, timeout):
    """pb to completion with bytes output (stdout/stderr/returncode), here or on the device."""
    if hasattr(binary, 'run_remote'):
        return binary.run_remote(arguments, timeout=timeout, text=False)
    return subprocess.run([str(binary), *arguments], capture_output=True, timeout=timeout)


def pb_text(binary, arguments, input=None, timeout=None, env=None, stdin=None):
    """pb to completion like subprocess.run(..., capture_output=True, text=True), here or on the device (where env
    is an overlay on the device's own environment and stdin is always what `input` holds, then end of input)."""
    if hasattr(binary, 'run_remote'):
        return binary.run_remote(arguments, input=input, env=env, timeout=timeout or 180)
    return subprocess.run([str(binary), *arguments], input=input, stdin=stdin, capture_output=True, text=True,
                          timeout=timeout, env=env)


WINDOWS, WINDOWS_TEAMMATE = 'windows', 'windows-teammate'


def is_side(command):
    """Whether a check's --owner-cmd/--member-cmd names the Windows device instead of a Unix command line:
    `windows` is pb.exe as the device's own account, `windows-teammate` as its second signed-in profile."""
    return command in (WINDOWS, WINDOWS_TEAMMATE)


def side_run(command, arguments, input=None, timeout=180):
    """Run pb.exe on the device for is_side(command), with no shell quoting; a text CompletedProcess."""
    prefix, env = [], None
    if not device():
        raise RuntimeError('the side `%s` is the Windows device: set PB_SMOKE_WINDOWS_PB to its pb.exe' % command)
    if command == WINDOWS_TEAMMATE:
        from teammate_profile import environment, profile_root
        prefix, env = ['--config', str(profile_root() / 'config.json')], environment(profile_root())
    return pb_path().run_remote([*prefix, *arguments], input=input, env=env, timeout=timeout)


@contextlib.contextmanager
def workdir(prefix):
    """A scratch directory on the device under test: a RemotePath there, else a local Path."""
    target = device()
    if target:
        with target.temp_dir(prefix) as directory:
            yield directory
    else:
        with tempfile.TemporaryDirectory(prefix=prefix) as directory:
            yield pathlib.Path(directory)


def device_path(value):
    """A path named on the command line of a check: it lives on the device under test (RemotePath) or here."""
    target = device()
    return RemotePath(target, value) if target else pathlib.Path(value)


def copy_file(source, target):
    if isinstance(target, RemotePath):
        target.device.copy(source, target)
    else:
        shutil.copy2(source, target)
        os.chmod(target, 0o700)


def write_pattern(path, block, repeat):
    """block repeated `repeat` times, without sending a large file over SSH."""
    if isinstance(path, RemotePath):
        path.write_pattern(block, repeat)
    else:
        path.write_bytes(block * repeat)


def write_random(path, size):
    """Random bytes (whole MiB); returns their sha256 hex."""
    if isinstance(path, RemotePath):
        return path.write_random(size)
    digest = hashlib.sha256()
    with path.open('wb') as handle:
        for _ in range(size >> 20):
            block = os.urandom(1 << 20)
            digest.update(block)
            handle.write(block)
    return digest.hexdigest()


def truncate(path, size):
    """A sparse file of the given size."""
    if isinstance(path, RemotePath):
        path.truncate(size)
    else:
        with path.open('wb') as handle:
            handle.truncate(size)


def device():
    """The Windows device when PB_SMOKE_WINDOWS_PB is set, else None (the check runs on a Unix device)."""
    exe = os.environ.get('PB_SMOKE_WINDOWS_PB')
    if not exe:
        return None
    path = pathlib.PureWindowsPath(exe)
    if not path.is_absolute() or path.name.lower() != 'pb.exe':
        raise ValueError('PB_SMOKE_WINDOWS_PB must be an absolute Windows pb.exe path')
    return _device(exe)


@functools.lru_cache(maxsize=None)
def _device(exe):  # one object per exe so its environment lookups are cached
    return WindowsDevice(pb_exe=exe)


def journal_when(path, on, off=()):
    """interrupt_when expression: the JSON file at path has every key of `on` true and every key of `off` not true."""
    tests = ['$j.%s' % key for key in on] + ['-not $j.%s' % key for key in off]
    return ("$j=try{Get-Content -Raw -LiteralPath %s -ErrorAction Stop|ConvertFrom-Json}catch{$null};$j -and %s"
            % (ps_quote(path), ' -and '.join(tests)))


def true_argv():
    """The command that exits 0 on this device itself, for `pb exec <this machine> -- ...`."""
    return ['cmd', '/c', 'exit', '0'] if device() else ['true']


def pb_path():
    """Replaces Path.home() / '.local/bin/pb' in the checks."""
    target = device()
    return WindowsPb(target) if target else pathlib.Path.home() / '.local/bin/pb'


def which(name):
    """shutil.which on the device under test: the tool as a runnable WindowsPb there, a Path here, None if absent."""
    target = device()
    if target:
        found = target.ps('which ' + name, WHICH, name=name)[1]
        return WindowsPb(target, found) if found else None
    found = shutil.which(name)
    return pathlib.Path(found) if found else None


def runtime_dir(default):
    target = device()
    return target.runtime_dir() if target else default
