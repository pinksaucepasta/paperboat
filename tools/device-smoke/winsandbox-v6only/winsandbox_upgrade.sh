#!/bin/bash
# In-place `pb install` check on Windows (X-38): a Windows Sandbox on victus (default NAT networking) installs OLD pb.exe,
# pairs, then runs `pb install` of NEW pb.exe over it (guest-upgrade.ps1). Needs a logged-in interactive desktop on victus.
# usage: winsandbox_upgrade.sh start|status|cmd <script.ps1> [s]|stop
# env: OLD_EXE, NEW_EXE (windows pb.exe files), TOKEN (one-shot enrollment token, never printed), PEER_NAME (a paired machine to exec),
#      MACHINE_NAME (name of the guest), API_URL (server URL, for cmd-captive.ps1), BOOT_SETTLE (seconds to wait after the sandbox boots before the first install, default 240), VIC (ssh wrapper, default vic.sh)
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
VIC=${VIC:-$HERE/vic.sh}; STAGE='C:\Users\pujan\rx38r'
vps() { enc=$(iconv -f utf-8 -t utf-16le | base64 -w0); "$VIC" "powershell -NoProfile -NonInteractive -EncodedCommand $enc" 2>&1 | grep -v -e '^#< CLIXML' -e '^<Objs' || true; }
put() { SSHPASS=$("$VIC" --password) sshpass -e scp -q -o StrictHostKeyChecking=accept-new "$1" "pujan@victus:C:/Users/pujan/rx38r/share/$2"; }
task() { # run a one-shot cmd file as the interactive user (wsb only starts in that session)
  vps <<PS
Unregister-ScheduledTask rx38r-job -Confirm:\$false -ea 0
Register-ScheduledTask rx38r-job -Action (New-ScheduledTaskAction -Execute cmd.exe -Argument '/c $STAGE\\$1') -Principal (New-ScheduledTaskPrincipal -UserId pujan -LogonType Interactive) | Out-Null
Start-ScheduledTask rx38r-job
PS
}
case ${1:-} in
start)
  : "${OLD_EXE:?}" "${NEW_EXE:?}" "${TOKEN:?}"
  T=$(mktemp -d); umask 077
  printf 'PEER_NAME=%s\nMACHINE_NAME=%s\nAPI_URL=%s\nBOOT_SETTLE=%s\n' "${PEER_NAME:?}" "${MACHINE_NAME:-rx38r-w}" "${API_URL:?server URL}" "${BOOT_SETTLE:-240}" > "$T/params.txt"; printf '%s' "$TOKEN" > "$T/token.txt"
  vps <<PS
Remove-Item -Recurse -Force $STAGE -ea 0; New-Item -ItemType Directory -Force $STAGE\\share\\out | Out-Null
if(-not (Get-Command wsb -ea 0)){ throw 'wsb missing: enable Windows Sandbox' }
\$wsb='<Configuration><VGpu>Disable</VGpu><Networking>Default</Networking><MemoryInMB>3072</MemoryInMB><ClipboardRedirection>Disable</ClipboardRedirection><MappedFolders><MappedFolder><HostFolder>$STAGE\\share</HostFolder><SandboxFolder>C:\\host</SandboxFolder><ReadOnly>false</ReadOnly></MappedFolder></MappedFolders></Configuration>'
Set-Content -Path $STAGE\\start.cmd -Value ('@echo off' + [Environment]::NewLine + 'wsb start --config "' + \$wsb + '" > $STAGE\\start.out 2>&1')
Set-Content $STAGE\\run.cmd '@echo off
for /f "tokens=1" %%i in (''wsb list'') do wsb exec --id %%i -c "powershell -NoProfile -ExecutionPolicy Bypass -File C:\\host\\guest-upgrade.ps1" -r System > C:\\Users\\pujan\\rx38r\\run.out 2>&1'
Set-Content $STAGE\\stop.cmd 'for /f "tokens=*" %%i in (''wsb list'') do wsb stop --id %%i'
PS
  put "$T/params.txt" params.txt; put "$T/token.txt" token.txt; put "$HERE/guest-upgrade.ps1" guest-upgrade.ps1
  put "$OLD_EXE" pb-old.exe; put "$NEW_EXE" pb-new.exe; rm -rf "$T"
  task start.cmd; sleep 25; vps <<<"Get-Content $STAGE\\start.out"; task run.cmd ;;
cmd)
  vps <<<"Remove-Item $STAGE\\share\\out\\cmd.done -ea 0"; put "$2" cmd.ps1; end=$((SECONDS + ${3:-300}))
  while [ $SECONDS -lt $end ]; do
    r=$(vps <<<"if(Test-Path $STAGE\\share\\out\\cmd.done){ 'finished'; Get-Content $STAGE\\share\\out\\cmd.log }")
    [ -n "$r" ] && { echo "$r" | tail -n +2; exit 0; }; sleep 5
  done; echo "guest did not finish $2 in ${3:-300}s" >&2; exit 1 ;;
status) vps <<<"Get-Content $STAGE\\share\\out\\guest.log -Tail ${LINES:-40}; Test-Path $STAGE\\share\\out\\guest.ready" ;;
stop)
  vps <<<"New-Item -ItemType File $STAGE\\share\\out\\guest.stop -Force | Out-Null; Start-Sleep 8; Get-Content $STAGE\\share\\out\\guest.log"
  task stop.cmd; sleep 15
  vps <<<"Unregister-ScheduledTask rx38r-job -Confirm:\$false -ea 0; Remove-Item -Recurse -Force $STAGE -ea 0; wsb list" ;;
*) echo "usage: $0 start|status|cmd|stop" >&2; exit 2 ;;
esac
