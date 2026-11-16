# Phase 2 (run in the guest with the driver's `cmd`): replace the IPv6-only lockdown by a full-tunnel WireGuard
# (AllowedIPs 0.0.0.0/0: the default IPv4 route goes through the tunnel; the bed masquerades it).
$ep = ((Get-Content C:\host\params.txt | ? { $_ -like 'ENDPOINT_V4=*' }) -split '=')[1]
Log 'remove lockdown'
Set-NetFirewallProfile -All -DefaultOutboundAction Allow
Get-NetFirewallRule -DisplayName 'x38q*' | Remove-NetFirewallRule
& 'C:\Program Files\WireGuard\wireguard.exe' /uninstalltunnelservice x38q; Start-Sleep 5
Get-NetAdapter | % { Set-DnsClientServerAddress -InterfaceIndex $_.ifIndex -ResetServerAddresses }
$gw = (Get-Content C:\gw.txt).Trim(); route delete $ep 2>&1 | Out-Null; route add 0.0.0.0 mask 0.0.0.0 $gw metric 15 | Out-Null
# same key and peer as the IPv6-only tunnel, now routing all IPv4 (the bed masquerades it, bed-nat.sh on)
$c = (Get-Content C:\host\x38q.conf -Raw) -replace 'Address = .*', 'Address = 10.38.0.2/32, fd38:0:0:1::2/128' -replace 'DNS = .*', 'DNS = 1.1.1.1' -replace 'AllowedIPs = .*', 'AllowedIPs = 0.0.0.0/0'
Set-Content C:\x38f.conf $c
& 'C:\Program Files\WireGuard\wireguard.exe' /installtunnelservice C:\x38f.conf; Start-Sleep 10
Log (route print -4 | Out-String -Width 200)
Log ('public IPv4 egress: ' + (& curl.exe -4 -s -m 15 $p['API_URL'] -o NUL -w 'code=%{http_code} remote=%{remote_ip}' ))
Log ('default route interface: ' + (Get-NetRoute -DestinationPrefix 0.0.0.0/0 | % { "$($_.InterfaceAlias) metric $($_.RouteMetric)" } | Out-String))
