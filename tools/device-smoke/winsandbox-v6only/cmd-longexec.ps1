# Start a 150 s `pb exec` W>L (counts to 150) in a task; result in C:\pbw\wl.txt (collect with cmd-ft-collect.ps1).
Remove-Item C:\pbw\wl.txt, C:\pbw\wl.done -ea 0
$act = New-ScheduledTaskAction -Execute cmd.exe -Argument "/c C:\pbw\pb.exe exec $peer -- sh /home/pbt/count150.sh > C:\pbw\wl.txt 2>&1 & echo rc=%errorlevel% >> C:\pbw\wl.txt & echo end > C:\pbw\wl.done"
Register-ScheduledTask pbw-wl -Force -Action $act -User pbw -Password $pbwPass -RunLevel Highest | Out-Null
Start-ScheduledTask pbw-wl; Log 'long exec W>L started (150 s)'
