#!/bin/bash
# X-38 network switch on a macpool Mac (own machine, nobody else is affected): install BUILD, pair into the account of LPB's machine,
# run mac-addr-switch.sh under a Mac-initiated exec to PEER, a Peer-initiated exec to the Mac, then collect the logs.
# usage: macpool-switch.sh <N>   env: BUILD (dir with pb-darwin-arm64), LPB (runs pb on a paired Linux machine, used to mint the token), PEER (default lanex2-l2),
#        OUT (log dir), MAC_NAME (default rx38r-m). Never kill `sleep` on the runner: the workflow's keep-alive step is a sleep, killing it ends the runner.
set -u
N=${1:?macpool number}; H=macpool-$N; HERE=$(cd "$(dirname "$0")" && pwd); SMOKE=$HERE/..; BUILD=${BUILD:?}; LPB=${LPB:?}; PEER=${PEER:-lanex2-l2}; OUT=${OUT:-/tmp}; NAME=${MAC_NAME:-rx38r-m}
log() { echo "$(date +%T) $*"; }
R() { ssh -o ConnectTimeout=20 $H "$@"; }
scp -q $BUILD/pb-darwin-arm64 $H:/tmp/pb-x38r; R 'chmod 755 /tmp/pb-x38r; perl -e "alarm 240; exec @ARGV" /tmp/pb-x38r install 2>&1 | tail -1'
j=$($LPB machine add --json --name $NAME)
umask 077; python3 - "$j" > $OUT/.mtok <<'PY'
import sys,json,re,urllib.request
d=json.loads(sys.argv[1])['data']['linux_macos_command']; m=re.search(r"(https?://[^/'\" ]+)/install\?p=([^'\" ]+)", d)
t=urllib.request.urlopen(f"{m.group(1)}/install?p={m.group(2)}",timeout=30).read().decode()
print(re.search(r"PAPERBOAT_ENROLLMENT_TOKEN='([^']*)'",t).group(1),end='')
PY
scp -q $OUT/.mtok $H:/tmp/x38r.tok; rm -f $OUT/.mtok
R 'chmod 600 /tmp/x38r.tok; export PATH=$HOME/.local/bin:$PATH; perl -e "alarm 240; exec @ARGV" pb pair --name '$NAME' --enrollment-token-file /tmp/x38r.tok --json 2>&1 | cut -c1-120; rm -f /tmp/x38r.tok'
sleep 30; log "doctor: $(R 'export PATH=$HOME/.local/bin:$PATH; pb doctor 2>&1 | head -1')"
scp -q $HERE/mac-addr-switch.sh $H:/tmp/x38r-as.sh; R 'nohup bash /tmp/x38r-as.sh '$PEER' >/dev/null 2>&1 </dev/null &'
for i in $(seq 1 30); do R 'test -f /tmp/x38r-as.up -o -f /tmp/x38r-as.done' 2>/dev/null && break; sleep 3; done
( cd $SMOKE && python3 - <<PY
import xplat_fleet as f
L=f.linux(); r=L.pb('exec','$NAME','--','sh','-c','for i in \$(seq 1 150); do echo \$i; sleep 1; done',timeout=400)
print('peer>Mac exec rc',r.rc,'lines',len([l for l in r.text.split() if l.isdigit()]), r.errtext[-150:])
PY
) > $OUT/macas-peer2m.txt 2>&1
for i in $(seq 1 60); do R 'test -f /tmp/x38r-as.done' 2>/dev/null && break; sleep 5; done
R 'cat /tmp/x38r-as.log' > $OUT/macas-mac.log 2>&1
log "collected"
