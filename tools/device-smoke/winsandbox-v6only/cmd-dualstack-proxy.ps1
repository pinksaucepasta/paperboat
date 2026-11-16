# Dual-stack: lift the IPv6-only lockdown (IPv4 default route back via the sandbox NAT, outbound allowed), keep the IPv6 tunnel;
# checks to $peers. Then a dead WinHTTP system proxy: doctor must name it; reset: doctor healthy again.
$gw = (Get-Content C:\gw.txt).Trim()
Set-NetFirewallProfile -All -DefaultOutboundAction Allow
Get-NetFirewallRule -DisplayName 'x38q*' -ea 0 | Remove-NetFirewallRule
route delete $ep 2>&1 | Out-Null; route add 0.0.0.0 mask 0.0.0.0 $gw metric 15 | Out-Null
Get-NetAdapter | ? Name -ne 'x38q' | % { Set-DnsClientServerAddress -InterfaceIndex $_.ifIndex -ResetServerAddresses }
Start-Sleep 25
Log (route print -4 | Out-String -Width 200)
Log ('curl -4 api: ' + (& curl.exe -4 -s -o NUL -m 8 -w 'code=%{http_code}' $p['API_URL']))
Log ('curl -6 api: ' + (& curl.exe -6 -s -o NUL -m 15 -w 'code=%{http_code} remote=%{remote_ip}' $p['API_URL']))
Log '== dual-stack pb doctor'; Log (PB 'doctor')
foreach ($m in $peers) { Log "== pb ping $m"; Log (PB "ping $m --count 3"); Log "== pb exec $m"; Log (PB "exec $m -- uname -a") }
Log '== dead WinHTTP proxy'; netsh winhttp set proxy proxy-server="http=127.0.0.1:9;https=127.0.0.1:9" | Out-Null
Start-Sleep 45; Log (PB 'doctor')
netsh winhttp reset proxy | Out-Null; Log '== proxy reset'
Start-Sleep 60; Log (PB 'doctor')
