#!/bin/bash
# X-38 macOS full-tunnel with the Mac as INITIATOR (fleet Mac test-m). Run under: flock fleet-mac.lock bash macft.sh; sleep 300
# Steps: install the build in place, WireGuard via brew, bed peer, tunnel up detached (mac-ft.sh), L>M and W>M execs while it is up, tear down, restore the Mac.
# env: BUILD (dir with pb-darwin-arm64), RX (name of a paired Linux machine, default rx38r-l), LPB (command that runs pb on it), OUT (log dir)
set -u
HERE=$(cd "$(dirname "$0")" && pwd); SMOKE=$HERE/..; SSH=~/.mac-vm/ssh.sh; SCP=~/.mac-vm/scp.sh; OUT=${OUT:-/tmp}; EP=${EP:-192.168.31.146:51899}
BUILD=${BUILD:?}; LPB=${LPB:?}
M() { timeout 120 $SSH vm "$@"; }
log() { echo "$(date +%T) $*"; }
# the fleet Mac's login keychain locks by itself; a locked one makes every pb on it degraded (doctor names it)
M 'read -r p; security unlock-keychain -p "$p" ~/Library/Keychains/login.keychain-db' < ~/.mac-vm/vm-login.pw >/dev/null 2>&1
log "install $BUILD on test-m"
( cd $SMOKE && python3 - <<PY
import xplat_fleet as f
print(f.install(f.mac(), '$BUILD/pb-darwin-arm64')['version'])
PY
) 2>&1 | tail -2
log "wireguard-tools"
M 'export PATH=/opt/homebrew/bin:$PATH HOMEBREW_NO_AUTO_UPDATE=1; which wg-quick >/dev/null || brew install wireguard-tools 2>&1 | tail -2; which wg-quick'
BEDPUB=$($HERE/bed-wg.sh down >/dev/null 2>&1; $HERE/bed-wg.sh up)
PUB=$(M 'export PATH=/opt/homebrew/bin:$PATH; umask 077; wg genkey > /tmp/x38r.key; wg pubkey < /tmp/x38r.key')
$HERE/bed-wg.sh peer "$PUB"
M "umask 077; printf '[Interface]\nPrivateKey = %s\nAddress = 10.38.7.2/32\nDNS = 1.1.1.1\n[Peer]\nPublicKey = $BEDPUB\nAllowedIPs = 0.0.0.0/0\nEndpoint = $EP\nPersistentKeepalive = 15\n' \"\$(cat /tmp/x38r.key)\" > /tmp/x38rw.conf"
$SCP $HERE/mac-ft.sh /tmp/x38r-mac-ft.sh >/dev/null
M 'nohup bash /tmp/x38r-mac-ft.sh >/dev/null 2>&1 </dev/null &'
log "tunnel script started; waiting for the Mac-initiated checks"
for i in $(seq 1 60); do M 'test -f /tmp/x38r-ft.up' 2>/dev/null && break; sleep 5; done
log "tunnel up, Mac-initiated checks done; running L>M and W>M"
{ echo "== L>M ping"; $LPB ping test-m --count 3 2>&1 | tail -3; echo "== L>M exec"; $LPB exec test-m -- uname -sr 2>&1 | tail -2; } > $OUT/macft-lm.txt 2>&1
( cd $SMOKE && python3 - <<PY
import xplat_fleet as f
W=f.windows()
print('== W>M ping'); print(W.pb('ping','test-m','--count','3').text[-300:])
print('== W>M exec'); r=W.pb('exec','test-m','--','uname','-sr'); print(r.rc, r.text[-120:])
PY
) >> $OUT/macft-lm.txt 2>&1
for i in $(seq 1 60); do M 'test -f /tmp/x38r-ft.done' 2>/dev/null && break; sleep 5; done
M 'cat /tmp/x38r-ft.log' > $OUT/macft-mac.log 2>&1
log "restore"
M 'export PATH=/opt/homebrew/bin:$PATH HOMEBREW_NO_AUTO_UPDATE=1; sudo -n wg-quick down /tmp/x38rw.conf 2>/dev/null; rm -f /tmp/x38r.key /tmp/x38rw.conf /tmp/x38r-mac-ft.sh /tmp/x38r-ft.* ; brew uninstall wireguard-tools wireguard-go 2>&1 | tail -1; route -n get default | grep -E "gateway|interface"'
$HERE/bed-wg.sh down
log "done"
