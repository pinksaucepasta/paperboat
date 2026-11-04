#!/bin/bash
# mac_console_fixtures.sh SUBCOMMAND [ARG]: macOS twin of the Docker fixtures in final_console_inspector.sh,
# final_tunnel_fixtures.sh and final_console_files.sh. Runs ON the Mac as the paired user (over ssh) and prints
# KEY=VALUE lines for the console checks. State (pids, ids) lives in ~/.pb-console-fixtures.
#   preview-up      origin on 127.0.0.1:8080 + background preview -> PREVIEW_ID PREVIEW_HOST
#   preview-down    stop that preview by id, kill the origin
#   tunnel-up       origin on 127.0.0.1:45992 + tunnel          -> TUNNEL_ID TUNNEL_NAME TUNNEL_PORT
#   tunnel-down     delete that tunnel (confirm token), kill the origin
#   files-up [PEER] no PEER: print SELF_ID SELF_ALIAS OFFLINE_ID FILE_NAME FILE_CONTENT FILE_PATH; the fixture file
#                   stays in ~/.pb-console-fixtures/ (send it from a peer: pb send FILE --to SELF_ALIAS).
#                   PEER (alias): send the fixture from this Mac to PEER and also print ONLINE_ID=<peer id>.
#   restart-runtime kickstart the carrier launchd agent (Linux: systemctl restart paperboat-carrier-u<uid>)
set -uo pipefail
export PATH="$HOME/.local/bin:/opt/homebrew/opt/python@3.12/libexec/bin:/opt/homebrew/bin:$PATH"
PB=$HOME/.local/bin/pb
D=$HOME/.pb-console-fixtures; mkdir -p "$D"
CARRIER_PREFIX=com.pinksaucepasta.paperboat.carrier
PORT_P=8080 PORT_T=45992

jf() { python3 -c 'import json,sys;d=json.loads(sys.stdin.read());d=d.get("data",d);print(eval(sys.argv[1],{},{"d":d}))' "$1"; }
origin_up() { # origin_up PORT NAME: http.server on loopback, pid file $D/NAME.pid
  curl -fs -o /dev/null "http://127.0.0.1:$1/" && return 0
  (cd "$D" && nohup python3 -m http.server "$1" --bind 127.0.0.1 >/dev/null 2>&1 & echo $! >"$D/$2.pid")
  for _ in 1 2 3 4 5 6 7 8 9 10; do curl -fs -o /dev/null "http://127.0.0.1:$1/" && return 0; sleep 1; done
  echo "FAIL origin on $1 did not start" >&2; return 1
}
origin_down() { [ -f "$D/$1.pid" ] && kill "$(cat "$D/$1.pid")" 2>/dev/null; rm -f "$D/$1.pid"; }
mine() { python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["machine_id"])' \
  "${PAPERBOAT_RUNTIME_STATE_ROOT:-$HOME/Library/Application Support/Paperboat/runtime}/machine-registration.json"; }

case ${1:-} in
preview-up)
  origin_up $PORT_P preview-origin || exit 1
  json=$(timeout 120 "$PB" preview $PORT_P --background --ttl 30m --json) || { echo "FAIL preview start" >&2; exit 1; }
  id=$(jf 'd["id"]' <<<"$json") || exit 1
  echo "$id" >"$D/preview.id"
  echo "PREVIEW_ID=$id"; echo "PREVIEW_HOST=$(jf 'd["endpoint"].split("//")[1]' <<<"$json")" ;;
preview-down)
  [ -f "$D/preview.id" ] && "$PB" preview stop "$(cat "$D/preview.id")" --json >/dev/null 2>&1
  rm -f "$D/preview.id"; origin_down preview-origin ;;
tunnel-up)
  origin_up $PORT_T tunnel-origin || exit 1
  name=final-mac-$(date +%s)
  id=$("$PB" tunnel create "$name" --port $PORT_T --wait --timeout 90s --json | jf 'd["tunnel"]["id"]') || { echo "FAIL tunnel create" >&2; exit 1; }
  echo "$id" >"$D/tunnel.id"
  echo "TUNNEL_ID=$id"; echo "TUNNEL_NAME=$name"; echo "TUNNEL_PORT=$PORT_T" ;;
tunnel-down)
  if [ -f "$D/tunnel.id" ]; then
    id=$(cat "$D/tunnel.id")
    tok=$("$PB" tunnel delete "$id" --json 2>/dev/null | jf 'd["confirmation_token"]') &&
      "$PB" tunnel delete "$id" --confirm "$tok" --json >/dev/null 2>&1
    rm -f "$D/tunnel.id"
  fi
  origin_down tunnel-origin ;;
files-up)
  stamp=$(date +%s); name=final-files-$stamp.txt; content=FINAL_FILES_$stamp
  list=$("$PB" machine list --json) || { echo "FAIL machine list" >&2; exit 1; }
  self=$(mine) || { echo "FAIL no machine id (registration file)" >&2; exit 1; }
  alias=$(python3 -c 'import json,sys;print(*[m["alias"] for m in json.loads(sys.argv[1])["machines"] if m["id"]==sys.argv[2]])' "$list" "$self")
  off=$(python3 -c 'import json,sys;o=[m["id"] for m in json.loads(sys.argv[1])["machines"] if not m["online"]];print(o[0] if o else "")' "$list")
  [ -n "$off" ] || { echo "FAIL need one offline machine on the account" >&2; exit 1; }
  printf %s "$content" >"$D/$name"
  if [ -n "${2:-}" ]; then
    peer_id=$(python3 -c 'import json,sys;print(*[m["id"] for m in json.loads(sys.argv[1])["machines"] if m["alias"]==sys.argv[2]])' "$list" "$2")
    (cd "$D" && "$PB" send "$name" --to "$2" --json >/dev/null) || { echo "FAIL send to $2" >&2; exit 1; }
    rm -f "$D/$name"; echo "ONLINE_ID=$peer_id"
  fi
  echo "SELF_ID=$self"; echo "SELF_ALIAS=$alias"; echo "OFFLINE_ID=$off"
  echo "FILE_NAME=$name"; echo "FILE_CONTENT=$content"; echo "FILE_PATH=$D/$name" ;;
restart-runtime)
  label=$(launchctl list | awk -v p="$CARRIER_PREFIX" 'index($3,p)==1{print $3; exit}')
  [ -n "$label" ] || { echo "FAIL no carrier launchd agent ($CARRIER_PREFIX*)" >&2; exit 1; }
  launchctl kickstart -k "gui/$(id -u)/$label" && echo "RESTARTED=$label" ;;
*) echo "usage: $0 preview-up|preview-down|tunnel-up|tunnel-down|files-up [PEER]|restart-runtime" >&2; exit 2 ;;
esac
