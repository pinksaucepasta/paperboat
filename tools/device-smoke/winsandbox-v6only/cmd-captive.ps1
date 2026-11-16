# Captive portal on the guest: the API host resolves to loopback where an HTTP listener answers every request with a 302 to a
# foreign host (what a Wi-Fi sign-in page does). Prints what `pb doctor` says while it is up and after it is removed.
$apiHost = ([uri]$p.API_URL).Host
$hosts = "$env:SystemRoot\System32\drivers\etc\hosts"; $orig = Get-Content $hosts -Raw
Add-Content $hosts "`n127.0.0.1 $apiHost"
ipconfig /flushdns | Out-Null
# raw TCP listener: HttpListener cannot share port 80 with the machine guard's listeners on 127.100.0.x
$job = Start-Job { $l = New-Object Net.Sockets.TcpListener ([Net.IPAddress]::Parse('127.0.0.1')), 80; $l.Start()
  while ($true) { $c = $l.AcceptTcpClient(); $s = $c.GetStream(); $buf = New-Object byte[] 4096; [void]$s.Read($buf, 0, 4096)
    $r = [Text.Encoding]::ASCII.GetBytes("HTTP/1.1 302 Found`r`nLocation: http://portal.example.test/login`r`nContent-Length: 0`r`nConnection: close`r`n`r`n"); $s.Write($r, 0, $r.Length); $c.Close() } }
Start-Sleep 5
Log ('listener job: ' + $job.State + ' ' + (Receive-Job $job -Keep 2>&1 | Out-String))
Log ('portal probe: ' + (& curl.exe -s -m 5 -o NUL -w 'code=%{http_code} location=%{redirect_url} remote=%{remote_ip}' "http://$apiHost/healthz"))
Start-Sleep 70
Log '== doctor with the portal up'; Log (PB 'doctor')
Remove-Job -Force $job; Set-Content $hosts $orig -NoNewline; ipconfig /flushdns | Out-Null
Start-Sleep 20
Log '== doctor 20 s after the portal was removed'; Log (PB 'doctor')
Log "== pb exec $peer"; Log (PB "exec $peer -- echo ok")
