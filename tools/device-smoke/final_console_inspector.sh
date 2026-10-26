#!/bin/bash
# final_console_inspector.sh HOST CONTAINER: the F17 console checks on the live console against a preview served from
# CONTAINER on Docker host HOST (user pbt, uid 1000): the inspector page (live list, filters, headers, replay, clear),
# a HOLD-second hold with no reconnect, and a carrier restart with the page open. Each gets its own fresh preview, which
# is stopped by exact id afterwards. Needs ORIGIN, STATE, PW_CHROMIUM, PW_MODULE (see console/scripts/run-console-check.mjs).
set -uo pipefail
HOST=${1:?docker host} CT=${2:?container}
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
DOCKER=${SMOKE_CT_DOCKER:-docker}
X() { ssh -o BatchMode=yes "$HOST" "$DOCKER exec -i -u pbt -w /home/pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 $CT bash -lc $(printf '%q' "$*")"; }
FAILED=0
ID= PHOST=
new_preview() { # new_preview: a fresh background preview of a marker origin; sets ID and PHOST
  X 'curl -fs -o /dev/null http://127.0.0.1:8080/ || (nohup python3 -m http.server 8080 >/dev/null 2>&1 &) ; sleep 1' >/dev/null
  local json
  json=$(X 'timeout 120 .local/bin/pb preview 8080 --background --ttl 30m --json') || return 1
  ID=$(python3 -c 'import json,sys;d=json.loads(sys.argv[1]);print(d.get("data",d)["id"])' "$json") || return 1
  PHOST=$(python3 -c 'import json,sys;d=json.loads(sys.argv[1]);print(d.get("data",d)["endpoint"].split("//")[1])' "$json")
}
stop_preview() { X ".local/bin/pb preview stop $ID --json >/dev/null 2>&1" || true; }
step() { # step NAME COMMAND...
  local name=$1 out; shift
  out=$("$@" 2>&1); local code=$?
  printf '%s %s: %s\n' "$([ $code = 0 ] && echo PASS || echo FAIL)" "$name" "$(grep -m2 FAIL <<<"$out" | tr '\n' ' ' ; tail -n1 <<<"$out")"
  [ $code = 0 ] || FAILED=1
}

new_preview || { echo "FAIL inspector-page: could not start a preview"; exit 1; }
step inspector-page node "$ROOT/console/scripts/run-console-check.mjs" "$ROOT/console/scripts/check-preview-inspector.js" "/previews/$ID/inspect" "{\"__previewHost\":\"$PHOST\"}"
stop_preview
new_preview && step inspector-hold env HOLD=${HOLD:-300} node "$ROOT/console/scripts/check-preview-inspector-hold.mjs" "$ID" "$PHOST"
stop_preview
new_preview && step inspector-restart env \
  RESTART_CMD="ssh $HOST '$DOCKER exec $CT systemctl restart paperboat-carrier-u1000.service'" \
  STOP_CMD="ssh $HOST \"$DOCKER exec -u pbt -w /home/pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 $CT .local/bin/pb preview stop <id> --json\" >/dev/null" \
  node "$ROOT/console/scripts/check-preview-inspector-restart.mjs" "$ID" "$PHOST"
stop_preview
exit $FAILED
