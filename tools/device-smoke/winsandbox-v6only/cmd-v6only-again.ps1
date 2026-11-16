# Back to strictly IPv6-only after cmd-fulltunnel.ps1: the IPv6-only tunnel, no IPv4 default route (host route to the endpoint
# only), DNS64, outbound IPv4 blocked except WireGuard's UDP to the bed; then the same proof and the peer checks.
$gw = (Get-Content C:\gw.txt).Trim()
& 'C:\Program Files\WireGuard\wireguard.exe' /uninstalltunnelservice x38f; Start-Sleep 5
& 'C:\Program Files\WireGuard\wireguard.exe' /installtunnelservice C:\x38q.conf; Start-Sleep 8
route add $ep mask 255.255.255.255 $gw metric 1 | Out-Null; route delete 0.0.0.0 | Out-Null
Get-NetAdapter | % { Set-DnsClientServerAddress -InterfaceIndex $_.ifIndex -ServerAddresses $dns }
Set-NetFirewallProfile -All -DefaultOutboundAction Block
New-NetFirewallRule -DisplayName 'x38q wg endpoint' -Direction Outbound -Action Allow -Protocol UDP -RemoteAddress "$ep" -RemotePort 51888 -ea 0 | Out-Null
New-NetFirewallRule -DisplayName 'x38q ipv6' -Direction Outbound -Action Allow -RemoteAddress 'fe80::/10', 'fd38:0:0:1::/64', '2000::/3', '64:ff9b::/96' -ea 0 | Out-Null
Start-Sleep 10
Log (route print -4 | Out-String -Width 200)
Log ('ping 8.8.8.8: ' + ((ping -n 1 -w 1500 8.8.8.8) -join ' '))
Log ('curl -4 api: ' + (& curl.exe -4 -s -o NUL -m 8 -w 'code=%{http_code}' https://api.pprbt.dev))
Log ('curl -6 api: ' + (& curl.exe -6 -s -o NUL -m 15 -w 'code=%{http_code} remote=%{remote_ip}' https://api.pprbt.dev))
Start-Sleep 30
Log '== pb doctor'; Log (PB 'doctor')
