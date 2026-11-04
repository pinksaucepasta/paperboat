#!/usr/bin/env bash
# Synthetic probe (R-06): one pass of exec, file send and a public preview request between two
# probe machines, using an already signed-in `pb` (PB_BIN, HOME/XDG of the probe account).
# Prints one JSON line per step and exits non-zero on the first failure.
#   PROBE_MACHINE_A / PROBE_MACHINE_B   names of the two probe machines (required)
#   PB_BIN                              pb binary (default: pb)
#   PROBE_STEP_TIMEOUT                  seconds per step (default 60)
set -uo pipefail
pb=${PB_BIN:-pb} a=${PROBE_MACHINE_A:?} b=${PROBE_MACHINE_B:?} limit=${PROBE_STEP_TIMEOUT:-60}
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
marker="probe-$(date +%s)-$$"

step() { # name command...
  local name=$1 start=$SECONDS; shift
  if timeout "$limit" "$@" >"$tmp/out" 2>"$tmp/err"; then
    printf '{"step":"%s","ok":true,"seconds":%d}\n' "$name" $((SECONDS - start))
  else
    printf '{"step":"%s","ok":false,"seconds":%d,"error":%s}\n' "$name" $((SECONDS - start)) \
      "$(head -c 300 "$tmp/err" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))')"
    exit 1
  fi
}

exec_echo() { [ "$("$pb" exec "$1" -- echo "$marker")" = "$marker" ]; }
send_file() {
  echo "$marker" >"$tmp/$marker.txt"
  "$pb" send "$tmp/$marker.txt" --to "$b" &&
    "$pb" exec "$b" -- sh -c "find \"\$HOME\" -name '$marker.txt' -print -quit | grep -q ."
}
public_preview() { # a static server and a preview of it on A, fetched from here without credentials
  "$pb" exec "$a" -- sh -c "mkdir -p /tmp/$marker && echo $marker >/tmp/$marker/index.html &&
    (cd /tmp/$marker && nohup python3 -m http.server 18099 >/dev/null 2>&1 & echo \$! >/tmp/$marker.pid)" || return 1
  "$pb" exec "$a" -- "$pb" preview 18099 --ttl 2m --background --json >"$tmp/preview.json" || return 1
  local url; url=$(python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));print(d.get("url") or d.get("data",{}).get("url",""))' "$tmp/preview.json")
  [ -n "$url" ] && [ "$(curl -fsS --max-time 20 "$url")" = "$marker" ]
}
cleanup_preview() { "$pb" exec "$a" -- sh -c "kill \$(cat /tmp/$marker.pid) 2>/dev/null; rm -rf /tmp/$marker /tmp/$marker.pid" >/dev/null 2>&1; }
trap 'cleanup_preview; rm -rf "$tmp"' EXIT

step exec_a exec_echo "$a"
step exec_b exec_echo "$b"
step send_file send_file
step public_preview public_preview
