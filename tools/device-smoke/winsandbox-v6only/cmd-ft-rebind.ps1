# Full-tunnel VPN on the guest (cmd-fulltunnel.ps1), checks, then a 150 s `pb exec` to the peer (counts to 150) left running in a
# task so the driver can rebind the NAT (bed-nat.sh rebind) underneath it. Output: cmd.log; the long exec writes C:\pbw\wl.txt.
. C:\host\cmd-fulltunnel.ps1
Start-Sleep 30
Log '== pb doctor (full tunnel)'; Log (PB 'doctor')
Log "== pb ping $peer"; Log (PB "ping $peer --count 3")
Log "== pb exec $peer"; Log (PB "exec $peer -- echo ok")
Remove-Item C:\pbw\wl.txt, C:\pbw\wl.done -ea 0
$act = New-ScheduledTaskAction -Execute cmd.exe -Argument "/c C:\pbw\pb.exe exec $peer -- sh /home/pbt/count150.sh > C:\pbw\wl.txt 2>&1 & echo rc=%errorlevel% >> C:\pbw\wl.txt & echo end > C:\pbw\wl.done"
Register-ScheduledTask pbw-wl -Force -Action $act -User pbw -Password $pbwPass -RunLevel Highest | Out-Null
Start-ScheduledTask pbw-wl; Log 'long exec W>L started (150 s)'
