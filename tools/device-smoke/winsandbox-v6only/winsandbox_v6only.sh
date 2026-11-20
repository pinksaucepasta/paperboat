#!/bin/bash
# Windows strict IPv6-only check (X-38): a Windows Sandbox on the Windows device reaches the world only through a WireGuard
# tunnel to a NAT64/DNS64 bed on a Linux host (the Linux test host). Replaces the hand-built bed of R-X-38-O.
# Needs: a logged-in interactive desktop on the Windows device (Windows Sandbox only starts there), SMOKE_WINDOWS_PASSWORD via vic.sh, ssh the Linux test host.
# Bed on the Linux test host (tayga NAT64 64:ff9b::/96, unbound DNS64 on fd38:0:0:1::1, wg interface x38ow) must already exist.
# The bed needs TCP MSS clamping on the WireGuard interface (both families, -i and -o x38ow, TCPMSS --clamp-mss-to-pmtu in
# the mangle FORWARD chain): without it HTTPS through NAT64 times out on the first full-size segment (tayga's +20 byte
# header exceeds the tunnel MTU) and pb install fails in the machine guard phase. bed-nat.sh does that and the IPv4 NAT.
# usage: winsandbox_v6only.sh start|status|cmd <script.ps1> [s]|stop
# env: PB_EXE (windows pb.exe), WG_MSI_URL, API_URL (server URL), TOKEN (a one-shot
# enrollment token; minted from the Linux peer when unset)
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
VIC=${VIC:-$HERE/vic.sh}               # ssh wrapper to the Windows device: vic.sh '<cmd>' (supplies the password)
BED=${BED:-$(python3 "$HERE/../dev_targets.py" update_test_host)}; WGIF=${WGIF:-x38ow}; EP4=${EP4:-$(python3 "$HERE/../dev_targets.py" sandbox_endpoint_ipv4)}; DNS6=${DNS6:-fd38:0:0:1::1}
TARGETS=$HERE/../dev_targets.py
WIN_HOME=$(python3 "$TARGETS" smoke SMOKE_WINDOWS_HOME)
WIN_LOGIN=$(python3 "$TARGETS" smoke SMOKE_WINDOWS_SSH)
WIN_USER=${WIN_LOGIN%@*}
: "${WIN_HOME:?configure the Windows home path}" "${WIN_USER:?configure the Windows SSH login}"
STAGE="$WIN_HOME/rx38q"
STAGE=${STAGE//\//\\}
LPB=${LPB:-'sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 rx38o-a /home/pbt/.local/bin/pb'}   # pb of the Linux peer, run on $BED
vps() { enc=$(iconv -f utf-8 -t utf-16le | base64 -w0); "$VIC" "powershell -NoProfile -NonInteractive -EncodedCommand $enc" 2>&1 | grep -v -e '^#< CLIXML' -e '^<Objs' || true; }
put() { "$VIC" --scp "$1" "$WIN_HOME/rx38q/share/$2"; }  # a pty line limit breaks stdin transfers
case ${1:-} in
start)
  : "${BED:?configure the Linux test host}" "${EP4:?configure the sandbox IPv4 endpoint}" "${PB_EXE:?windows pb.exe}" "${WG_MSI_URL:?official wireguard-amd64 msi URL}"
  # one-shot enrollment token for the new machine, minted by the Linux peer (pb machine add) and read from the installer
  # page it returns; never printed. TOKEN=... overrides.
  if [ -z "${TOKEN:-}" ]; then
    TOKEN=$(ssh "$BED" "$LPB machine add --json --name ${MACHINE_NAME:-rx38q-w}" | python3 -c 'import sys,json,re; print(re.search(r"PB_TOKEN=\x27([^\x27]+)\x27",json.load(sys.stdin)["data"]["windows_command"]).group(1))')
  fi
  umask 077; T=$(mktemp -d)
  # client key pair and bed peer; the bed adds the peer, the private key only travels into the conf below.
  ssh "$BED" "k=\$(wg genkey); echo \$k; echo \$k | wg pubkey" > "$T/keys"
  PRIV=$(sed -n 1p "$T/keys"); PUB=$(sed -n 2p "$T/keys"); SRV=$(ssh "$BED" "sudo -n wg show $WGIF public-key")
  ssh "$BED" "sudo -n wg set $WGIF peer '$PUB' allowed-ips 10.38.0.2/32,fd38:0:0:1::2/128; echo '$PUB' > /tmp/x38q.peer"
  # AllowedIPs: not ::/0 (WireGuard would then block all IPv4 on the guest, including the endpoint path).
  cat > "$T/x38q.conf" <<CONF
[Interface]
PrivateKey = $PRIV
Address = fd38:0:0:1::2/128
DNS = $DNS6
[Peer]
PublicKey = $SRV
AllowedIPs = 2000::/3, 64:ff9b::/96, fd38:0:0:1::/64
Endpoint = $EP4:51888
PersistentKeepalive = 15
CONF
  printf 'ENDPOINT_V4=%s\nDNS6=%s\nAPI_URL=%s\nPEER_NAME=%s\nMACHINE_NAME=%s\n' "$EP4" "$DNS6" "${API_URL:?server URL}" "${PEER_NAME:-rx38o-a}" "${MACHINE_NAME:-rx38q-w}" > "$T/params.txt"
  printf '%s' "$TOKEN" > "$T/token.txt"
  vps <<PS
Remove-Item -Recurse -Force $STAGE -ea 0; New-Item -ItemType Directory -Force $STAGE\\share\\out | Out-Null
if(-not (Get-Command wsb -ea 0)){ throw 'wsb missing: enable Windows Sandbox' }
Invoke-WebRequest '$WG_MSI_URL' -OutFile $STAGE\\share\\wireguard.msi -UseBasicParsing
PS
  REM="$STAGE\\share"
  # Copy fixtures through the same configured Windows SSH login.
  for f in "$T/x38q.conf" "$T/params.txt" "$T/token.txt" "$HERE/guest-v6only.ps1"; do put "$f" "$(basename "$f")"; done
  put "$PB_EXE" pb.exe
  rm -rf "$T"
  vps <<PS
\$wsb='<Configuration><VGpu>Disable</VGpu><Networking>Default</Networking><MemoryInMB>3072</MemoryInMB><ClipboardRedirection>Disable</ClipboardRedirection><MappedFolders><MappedFolder><HostFolder>$STAGE\\share</HostFolder><SandboxFolder>C:\\host</SandboxFolder><ReadOnly>false</ReadOnly></MappedFolder></MappedFolders></Configuration>'
Set-Content -Path $STAGE\\start.cmd -Value ('@echo off' + [Environment]::NewLine + 'wsb start --config "' + \$wsb + '" > $STAGE\\start.out 2>&1')
Unregister-ScheduledTask rx38q-job -Confirm:\$false -ea 0
Register-ScheduledTask rx38q-job -Action (New-ScheduledTaskAction -Execute cmd.exe -Argument '/c $STAGE\\start.cmd') -Principal (New-ScheduledTaskPrincipal -UserId '$WIN_USER' -LogonType Interactive) | Out-Null
Start-ScheduledTask rx38q-job; Start-Sleep 20; Get-Content $STAGE\\start.out
# run the guest script as System in the sandbox (detached: it keeps running as the check driver)
Set-Content $STAGE\\run.cmd '@echo off
for /f "tokens=1" %%i in (''wsb list'') do wsb exec --id %%i -c "powershell -NoProfile -ExecutionPolicy Bypass -File C:\\host\\guest-v6only.ps1" -r System > $STAGE\\run.out 2>&1'
Unregister-ScheduledTask rx38q-job -Confirm:\$false
Register-ScheduledTask rx38q-job -Action (New-ScheduledTaskAction -Execute cmd.exe -Argument '/c $STAGE\\run.cmd') -Principal (New-ScheduledTaskPrincipal -UserId '$WIN_USER' -LogonType Interactive) | Out-Null
Start-ScheduledTask rx38q-job
PS
  ;;
cmd)  # run a PowerShell script in the guest and print its output: cmd <script.ps1> [max seconds]
  put "$2" cmd.ps1; end=$((SECONDS + ${3:-300}))
  while [ $SECONDS -lt $end ]; do
    r=$(vps <<PS
if(Test-Path $STAGE\\share\\out\\cmd.done){ 'finished'; Get-Content $STAGE\\share\\out\\cmd.log }
PS
)
    [ -n "$r" ] && { echo "$r" | tail -n +2; exit 0; }; sleep 5
  done; echo "guest did not finish $2 in ${3:-300}s" >&2; exit 1 ;;
status) vps <<PS
Get-Content $STAGE\\share\\out\\guest.log -Tail ${LINES:-40}
Test-Path $STAGE\\share\\out\\guest.ready
PS
  ;;
stop)  # stop the guest script, copy the log, stop the sandbox, remove the task, the staging dir and the bed peer
  vps <<PS
New-Item -ItemType File $STAGE\\share\\out\\guest.stop -Force | Out-Null; Start-Sleep 8
Get-Content $STAGE\\share\\out\\guest.log
PS
  vps <<PS
Unregister-ScheduledTask rx38q-job -Confirm:\$false -ea 0
Set-Content $STAGE\\stop.cmd 'for /f "tokens=*" %%i in (''wsb list'') do wsb stop --id %%i'
Register-ScheduledTask rx38q-stop -Action (New-ScheduledTaskAction -Execute cmd.exe -Argument '/c $STAGE\\stop.cmd') -Principal (New-ScheduledTaskPrincipal -UserId '$WIN_USER' -LogonType Interactive) | Out-Null
Start-ScheduledTask rx38q-stop; Start-Sleep 15; Unregister-ScheduledTask rx38q-stop -Confirm:\$false
Remove-Item -Recurse -Force $STAGE -ea 0; wsb list
PS
  ssh "$BED" "sudo -n wg set $WGIF peer \$(cat /tmp/x38q.peer) remove; rm -f /tmp/x38q.peer" ;;
*) echo "usage: $0 start|status|stop" >&2; exit 2 ;;
esac
