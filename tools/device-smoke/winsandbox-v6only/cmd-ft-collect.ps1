# After the rebind: wait for the long exec W>L to finish and report its output (line count, rc) and the path state both ways.
for ($i = 0; $i -lt 150 -and -not (Test-Path C:\pbw\wl.done); $i++) { Start-Sleep 2 }
$wl = Get-Content C:\pbw\wl.txt
Log ("long exec W>L: lines=" + ($wl | ? { $_ -match '^\d+$' }).Count + " last=" + ($wl | select -Last 2) -join ' ')
Log '== pb ping (path after the rebind)'; Log (PB "ping $peer --count 3")
Log '== pb doctor'; Log (PB 'doctor')
