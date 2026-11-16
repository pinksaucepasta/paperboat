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
$ep = $p.ENDPOINT_V4; $peer = $p.PEER_NAME; $name = $p.MACHINE_NAME; $dns = $p.DNS6

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
Run 'local administrator pbw + pb.exe' {
  net user pbw $pbwPass /add; net localgroup Administrators pbw /add
  New-Item -ItemType Directory -Force C:\pbw | Out-Null; Copy-Item C:\host\pb.exe C:\pbw\pb.exe
  icacls C:\pbw /grant 'pbw:(OI)(CI)F' | Out-Null
}
Run 'install WireGuard' { Start-Process msiexec -ArgumentList '/i C:\host\wireguard.msi /qn /norestart' -Wait; Test-Path 'C:\Program Files\WireGuard\wireguard.exe' }
Copy-Item C:\host\x38q.conf C:\x38q.conf
Run 'tunnel service' { & 'C:\Program Files\WireGuard\wireguard.exe' /installtunnelservice C:\x38q.conf; Start-Sleep 8; Get-Service 'WireGuardTunnel$x38q' | ft -auto }
Run 'ping the bed over the tunnel' { ping -6 -n 2 $dns }

# Strictly IPv6 only: drop every IPv4 route the sandbox NAT gave, block outbound IPv4 except WireGuard's UDP to the bed.
Run 'drop IPv4 default route (keep a host route to the tunnel endpoint)' {
  $gw = (Get-NetRoute -DestinationPrefix 0.0.0.0/0 | Sort-Object RouteMetric | select -First 1).NextHop; $gw | Out-File C:\gw.txt
  route add $ep mask 255.255.255.255 $gw metric 1; route delete 0.0.0.0
}
Run 'DNS64 on every adapter' { Get-NetAdapter | % { Set-DnsClientServerAddress -InterfaceIndex $_.ifIndex -ServerAddresses $dns } }
Run 'firewall: outbound default block' {
  Set-NetFirewallProfile -All -DefaultOutboundAction Block
  New-NetFirewallRule -DisplayName 'x38q wg endpoint' -Direction Outbound -Action Allow -Protocol UDP -RemoteAddress "$ep" -RemotePort 51888 | Out-Null
  New-NetFirewallRule -DisplayName 'x38q ipv6' -Direction Outbound -Action Allow -RemoteAddress 'fe80::/10', 'fd38:0:0:1::/64', '2000::/3', '64:ff9b::/96' | Out-Null
  Get-NetFirewallRule -DisplayName 'x38q*' | ft DisplayName, Enabled, Action -auto
}
Run 'proof: IPv4 unusable, IPv6 usable (guest, same moment)' {
  'route print -4'; route print -4
  'ping 8.8.8.8'; ping -n 1 -w 1500 8.8.8.8
  'ping the Ethernet gateway'; ping -n 1 -w 1500 (Get-Content C:\gw.txt).Trim()
  'curl -4 api'; & curl.exe -4 -s -o NUL -m 8 -w 'code=%{http_code}\n' $p['API_URL']
  'curl -4 1.1.1.1'; & curl.exe -s -o NUL -m 8 -w 'code=%{http_code}\n' https://1.1.1.1/
  'curl -6 api (DNS64/NAT64)'; & curl.exe -6 -s -o NUL -m 15 -w 'code=%{http_code} remote=%{remote_ip}\n' $p['API_URL']
  'ipconfig'; ipconfig
}

Log '== pb version'; Log (PB 'version')
Log '== pb install'; Log (PB 'install' 240)
Log '== pb pair'; Log (PB ("pair --name $name --enrollment-token " + (Get-Content C:\host\token.txt -Raw).Trim() + ' --json') 240)
Log 'settle 60 s'; Start-Sleep 60
Log '== pb doctor'; Log (PB 'doctor')
Log "== pb ping $peer"; Log (PB "ping $peer --count 3")
Log "== pb exec $peer"; Log (PB "exec $peer -- echo ok")
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
