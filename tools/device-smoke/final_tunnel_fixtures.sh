#!/bin/bash
# final_tunnel_fixtures.sh MODE HOST CT_A CT_B: final re-check entries whose check needs a live tunnel first.
# The tunnel and its origin are created here on the paired hp containers and deleted on every exit path.
#
#   private-tcp      C126: CT_B serves a TCP echo behind a private tunnel with a tcp_private route; CT_A runs
#                    tunnel_private_access.py against it (both selectors, exact bytes, listener closes).
#   console-tunnels  F8-08: CT_A serves HTTP behind a tunnel; console/scripts/check-tunnels.js runs on the live
#                    console (ORIGIN, STATE, PW_CHROMIUM from the caller).
#   domain-binding   F8-02: as console-tunnels, then check-domain-binding-driver.mjs adds a fresh hostname under
#                    SMOKE_DNS_ZONE and creates/deletes its DNS records through the fixtures endpoint (FINAL_FIXTURES_URL, token in FINAL_FIXTURES_TOKEN_FILE, never printed).
set -uo pipefail
MODE=${1:?private-tcp|console-tunnels|domain-binding} HOST=${2:?host} CT_A=${3:?container a} CT_B=${4:?container b}
HERE=$(cd "$(dirname "$0")" && pwd) ROOT=$(cd "$HERE/../.." && pwd)
STAMP=$(date +%s) ECHO_PORT=45991 HTTP_PORT=45992
TUNNEL_CT= TUNNEL_ID=

in_ct() { local ct=$1; shift; ssh -o BatchMode=yes "$HOST" "sudo -n docker exec -i -u pbt -w /home/pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 $ct bash -lc $(printf %q "$*")"; }
json_field() { python3 -c 'import json,sys;d=json.load(sys.stdin);d=d.get("data",d);print(eval(sys.argv[1],{},{"d":d}))' "$1"; }

cleanup() {
  if [ -n "$TUNNEL_ID" ]; then
    token=$(in_ct "$TUNNEL_CT" ".local/bin/pb tunnel delete $TUNNEL_ID --json" 2>/dev/null | json_field 'd["confirmation_token"]' 2>/dev/null)
    [ -n "$token" ] && in_ct "$TUNNEL_CT" ".local/bin/pb tunnel delete $TUNNEL_ID --confirm $token --json" >/dev/null 2>&1
  fi
  for ct in "$CT_A" "$CT_B"; do in_ct "$ct" "pkill -f '^python3 -c .*final-fixture-$STAMP' || true" >/dev/null 2>&1; done
}
trap cleanup EXIT

# One origin per mode, tagged with STAMP so cleanup finds exactly it.
start_http() { in_ct "$1" "nohup python3 -c 'import http.server,sys;sys.argv[0]=\"final-fixture-$STAMP\";http.server.test(HandlerClass=http.server.SimpleHTTPRequestHandler,port=$HTTP_PORT,bind=\"127.0.0.1\")' final-fixture-$STAMP >/dev/null 2>&1 &"; }
start_echo() { in_ct "$1" "nohup python3 -c '
import socket,sys,threading
sys.argv[0]=\"final-fixture-$STAMP\"
s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind((\"127.0.0.1\",$ECHO_PORT));s.listen()
def h(c):
    while True:
        d=c.recv(65536)
        if not d: break
        c.sendall(d)
while True:
    c,_=s.accept();threading.Thread(target=h,args=(c,),daemon=True).start()
' final-fixture-$STAMP >/dev/null 2>&1 &"; }

create_tunnel() { # ct name [--private]
  TUNNEL_CT=$1
  TUNNEL_ID=$(in_ct "$1" ".local/bin/pb tunnel create $2 --port $HTTP_PORT $3 --wait --timeout 90s --json" | json_field 'd["tunnel"]["id"]') || return 1
  [ -n "$TUNNEL_ID" ]
}

case $MODE in
private-tcp)
  start_http "$CT_B"; start_echo "$CT_B"; sleep 1
  create_tunnel "$CT_B" "final-ptcp-$STAMP" --private || { echo "FAIL private tunnel create"; exit 1; }
  route=$(in_ct "$CT_B" ".local/bin/pb tunnel route add $TUNNEL_ID --name echo --protocol tcp_private --to tcp://127.0.0.1:$ECHO_PORT --wait --timeout 90s --json" | json_field 'd["route"]["id"]') || { echo "FAIL tcp_private route add"; exit 1; }
  tar czf - -C "$HERE" tunnel_private_access.py pairing_lifecycle.py | ssh "$HOST" "sudo -n docker exec -i -u pbt $CT_A bash -c 'mkdir -p ~/ds && tar xzf - -C ~/ds'"
  in_ct "$CT_A" "cd ~/ds && python3 tunnel_private_access.py --tunnel $TUNNEL_ID --route $route --nonce final-$STAMP"
  ;;
console-tunnels)
  start_http "$CT_A"; sleep 1
  name=final-f808-$STAMP
  create_tunnel "$CT_A" "$name" || { echo "FAIL tunnel create"; exit 1; }
  (cd "$ROOT" && node console/scripts/run-console-check.mjs console/scripts/check-tunnels.js /tunnels "{\"__tunnel\":\"$name\",\"__port\":\"$HTTP_PORT\"}")
  ;;
domain-binding)
  : "${FINAL_FIXTURES_URL:?fixtures endpoint}" "${FINAL_FIXTURES_TOKEN_FILE:?its token file}" "${SMOKE_DNS_ZONE:?DNS zone for the test hostname}"
  start_http "$CT_A"; sleep 1
  create_tunnel "$CT_A" "final-f802-$STAMP" || { echo "FAIL tunnel create"; exit 1; }
  (cd "$ROOT" && CHECK=console/scripts/check-domain-binding.js node console/scripts/check-domain-binding-driver.mjs "$TUNNEL_ID" "final-$STAMP.$SMOKE_DNS_ZONE" "$SMOKE_DNS_ZONE")
  ;;
*) echo "unknown mode $MODE"; exit 2 ;;
esac
