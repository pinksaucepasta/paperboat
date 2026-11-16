# In-place upgrade check (X-38 part 3): installs OLD pb.exe, pairs, then runs `pb install` of the NEW pb.exe over it, in a Windows Sandbox with default (dual-stack NAT) networking.
# Runs inside a Windows Sandbox as SYSTEM (`wsb exec -r System`; the logon account is not an administrator, and pb as SYSTEM
# cannot start its service, so pb commands run as a local administrator `pbw` through elevated one-shot tasks, see PB below).
# Files in C:\host (mapped from the victus host staging dir):
#   wireguard.msi, pb.exe, x38q.conf (v6-only WireGuard client), token.txt (one-shot pb enrollment token), params.txt (KEY=VALUE).
# Makes the guest strictly IPv6-only (no IPv4 route, outbound IPv4 blocked except the tunnel endpoint), then pairs pb and checks it.
# Output: C:\host\out\guest.log; C:\host\out\guest.ready when the checks are done; then it waits for C:\host\cmd.ps1 files
# (run with `. cmd.ps1`, output C:\host\out\cmd.log, marker cmd.done) until C:\host\out\guest.stop.
$ErrorActionPreference = 'Continue'
$out = 'C:\host\out'; New-Item -ItemType Directory -Force $out | Out-Null
$log = "$out\guest.log"
function Log($m) { "$([DateTime]::Now.ToString('HH:mm:ss')) $m" | Tee-Object -FilePath $log -Append }
function Run($label, [scriptblock]$b) { Log "== $label"; (& $b 2>&1 | Out-String -Width 200) | Tee-Object -FilePath $log -Append }
$p = @{}; Get-Content C:\host\params.txt | % { $k, $v = $_ -split '=', 2; $p[$k] = $v }
$peer = $p.PEER_NAME; $name = $p.MACHINE_NAME

# pb as the local administrator pbw: an elevated one-shot task runs `C:\pbw\pb.exe <args>`; output is returned.
$pbwPass = -join ((48..57) + (65..90) + (97..122) | Get-Random -Count 10 | % { [char]$_ }) + 'aA1!'
function PB([string]$a, [int]$timeout = 180) {
  $f = "C:\pbw\run-$([guid]::NewGuid().ToString('N')).txt"
  $act = New-ScheduledTaskAction -Execute cmd.exe -Argument "/c C:\pbw\pb.exe $a > $f 2>&1 & echo rc=%errorlevel% >> $f & echo end >> $f.done"
  Register-ScheduledTask pbw-run -Force -Action $act -User pbw -Password $pbwPass -RunLevel Highest | Out-Null
  Start-ScheduledTask pbw-run
  for ($i = 0; $i -lt $timeout -and -not (Test-Path "$f.done"); $i++) { Start-Sleep 1 }
  Unregister-ScheduledTask pbw-run -Confirm:$false -ea 0
  if (Test-Path $f) { Get-Content $f | Out-String -Width 200 } else { "no output within ${timeout}s" }
}

Log "user=$env:USERNAME"
Run 'local administrator pbw + pb.exe (OLD build)' {
  net user pbw $pbwPass /add; net localgroup Administrators pbw /add
  New-Item -ItemType Directory -Force C:\pbw | Out-Null; Copy-Item C:\host\pb-old.exe C:\pbw\pb.exe
  icacls C:\pbw /grant 'pbw:(OI)(CI)F' | Out-Null
}
Log '== old pb version'; Log (PB 'version')
# A first install within ~4 min of the Sandbox booting failed with 'Access is denied' / sharing violations on Program Files directories (X-38-R, 3 of 3);
# it passed later. BOOT_SETTLE (params.txt) lets the guest finish starting first.
Start-Sleep ([int]$p.BOOT_SETTLE)
# Process Monitor (official Sysinternals download) records the first install: an elevated step failing with "Access is denied" shows as ACCESS DENIED by helper.exe.
Invoke-WebRequest https://live.sysinternals.com/Procmon64.exe -OutFile C:\pm.exe -UseBasicParsing
Start-Process C:\pm.exe -ArgumentList '/AcceptEula /Quiet /Minimized /BackingFile C:\pm.pml'; Start-Sleep 8
# The first install can fail on the guest's flaky egress ("read the local access domain from the Paperboat server ... Client.Timeout");
# install is retried (it is idempotent) and pairing runs only after an install succeeded, so no half-finished enrollment is left behind.
$installed = $false
foreach ($try in 1..3) { Log "== old pb install (try $try)"; $o = PB 'install' 300; Log $o; if ($o -match 'Installed Paperboat') { $installed = $true; break }; Start-Sleep 20 }
Start-Process C:\pm.exe -ArgumentList '/Terminate' -Wait; Start-Sleep 3
if ($installed) { Log '== old pb pair'; Log (PB ("pair --name $name --enrollment-token " + (Get-Content C:\host\token.txt -Raw).Trim() + ' --json') 300) } else { Log 'install failed 3 times: pair skipped' }
Log 'settle 90 s'; Start-Sleep 90
Log '== old pb doctor'; Log (PB 'doctor')
Log "== old pb exec $peer"; Log (PB "exec $peer -- echo ok")
Run 'swap in NEW pb.exe' { Stop-Process -Name pb -Force -ea 0; Copy-Item C:\host\pb-new.exe C:\pbw\pb.exe -Force }
Log '== new pb version'; Log (PB 'version')
$t0 = Get-Date
Log '== NEW pb install over the old install'; Log (PB 'install' 600)
Log ("install took " + [int]((Get-Date) - $t0).TotalSeconds + " s")
Log '== new pb doctor'; Log (PB 'doctor')
Log "== new pb exec $peer"; Log (PB "exec $peer -- echo ok")
Run 'services' { Get-Service | ? { $_.Name -match 'aperboat|pb' } | ft -auto }
'ready' | Out-File "$out\guest.ready"
while (-not (Test-Path "$out\guest.stop")) {
  if (Test-Path C:\host\cmd.ps1) {
    Move-Item -Force C:\host\cmd.ps1 C:\host\cmd.run.ps1; Remove-Item "$out\cmd.done" -ea 0
    $log = "$out\cmd.log"; Log '== cmd.ps1'; try { . C:\host\cmd.run.ps1 } catch { Log "cmd.ps1 failed: $_" }
    $log = "$out\guest.log"; 'done' | Out-File "$out\cmd.done"
  }
  Start-Sleep 3
}
Log 'stop requested'
'done' | Out-File "$out\guest.done"
