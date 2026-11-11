#!/usr/bin/env bash
# Production canary (R-06): one pass of the core paths between two dedicated canary machines A and B.
# Every step runs (one failure does not hide the others); one JSON line per step; exit 1 if any failed.
#   RUN_A / RUN_B        command prefix that runs its trailing arguments on A / B as the machine's pb user,
#                        e.g. 'sudo -n docker exec -i -u pbt -e HOME=/home/pbt pb-canary-a' (required)
#   CANARY_A / CANARY_B  machine names (required)
#   CANARY_TUNNEL_URL    stable endpoint of the canary tunnel to A's origin (required)
#   CANARY_PORT          origin port on A serving ~/canary-www (default 18099)
#   PB                   pb path on the machines (default .local/bin/pb, relative to the user's home)
#   PROBE_STEP_TIMEOUT   seconds per step (default 45)
#   PROBE_STEPS          only these steps, space separated (default: all); staging has no edge, so it runs
#                        exec_a_to_b exec_b_to_a ssh_a_to_b (deploy/staging-canary.sh)
#   CANARY_EDGES         every managed edge address (space separated); each must serve the canary tunnel
#                        directly, so an edge that dropped out of DNS or was null-routed still fails a step
set -uo pipefail
a=${CANARY_A:?} b=${CANARY_B:?} tunnel=${CANARY_TUNNEL_URL:-} port=${CANARY_PORT:-18099}
pb=${PB:-.local/bin/pb} limit=${PROBE_STEP_TIMEOUT:-45}
: "${RUN_A:?}" "${RUN_B:?}"
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
marker="canary-$(date +%s)-$$"
failed=0

# on A|B <script>: runs the script on that machine under a remote timeout (local timeout as the backstop).
on() { local run=$RUN_A; [ "$1" = B ] && run=$RUN_B; printf 'cd; %s\n' "$2" | timeout $((limit + 10)) $run timeout "$limit" sh -s; }

step() { # name function: the function's stdout/stderr land in $tmp/out, $tmp/err
  local name=$1 start=$SECONDS; shift
  [ -z "${PROBE_STEPS:-}" ] || [[ " $PROBE_STEPS " == *" $name "* ]] || return 0
  if "$@" >"$tmp/out" 2>"$tmp/err"; then
    printf '{"step":"%s","ok":true,"seconds":%d}\n' "$name" $((SECONDS - start))
  else
    failed=1
    printf '{"step":"%s","ok":false,"seconds":%d,"error":%s}\n' "$name" $((SECONDS - start)) \
      "$(tail -c 300 "$tmp/err" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read().strip()))')"
  fi
}
want() { grep -qxF "$marker" "$tmp/got" || { echo "expected the run marker, got: $(head -c 120 "$tmp/got")" >&2; return 1; }; }
fetch_all() { # url: every edge address in DNS must serve the run marker (DNS alone would test one at random)
  local host=${1#*://} ip bad=; host=${host%%/*}; host=${host%%:*}
  local ips; ips=$(getent ahostsv4 "$host" | awk '{print $1}' | sort -u)
  [ -n "$ips" ] || { echo "no address for $host" >&2; return 1; }
  for ip in $ips; do
    curl -fsS --max-time 10 --resolve "$host:443:$ip" "$1/" >"$tmp/got" 2>"$tmp/curl" && want 2>>"$tmp/curl" ||
      bad="$bad edge $ip: $(tail -c 120 "$tmp/curl" | tr '\n' ' ')"
  done
  [ -z "$bad" ] || { echo "$bad" >&2; return 1; }
}

origin() { on A "mkdir -p canary-www && echo $marker >canary-www/index.html && curl -fsS --max-time 5 http://127.0.0.1:$port/" >"$tmp/got" && want; }
exec_ab() { on A "$pb exec $b -- echo $marker" >"$tmp/got" && want; }
exec_ba() { on B "$pb exec $a -- echo $marker" >"$tmp/got" && want; }
ssh_ab() { on A "$pb ssh $b -- echo $marker" >"$tmp/got" && want; }
send_ab() { # A sends a file, B finds it in its Inbox with the marker inside, then removes it
  on A "echo $marker >/tmp/$marker.txt && $pb send /tmp/$marker.txt --to $b >/dev/null; rc=\$?; rm -f /tmp/$marker.txt; exit \$rc" &&
    on B "i=\$($pb inbox path --json | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d.get(\"data\",d)[\"path\"])') || exit 1
for _ in \$(seq 20); do f=\$(find \"\$i\" -name '$marker.txt' -print -quit); [ -n \"\$f\" ] && break; sleep 1; done
[ -n \"\$f\" ] || { echo 'file not in the Inbox after 20 s' >&2; exit 1; }
cat \"\$f\"; rm -f \"\$f\"" >"$tmp/got" && want
}
preview() { # a new preview of A's origin, fetched from here without credentials, then stopped
  on A "$pb preview $port --background --ttl 2m --json" >"$tmp/preview.json" || return 1
  local id url
  id=$(python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));d=d.get("data",d);print(d["id"])' "$tmp/preview.json") &&
    url=$(python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));d=d.get("data",d);print(d.get("url") or d["endpoint"])' "$tmp/preview.json") ||
    { echo "unexpected preview answer: $(head -c 200 "$tmp/preview.json")" >&2; return 1; }
  local rc _ # a new route reaches the edges within seconds (check-edges.sh allows 30 s); 20 s bound here
  for _ in 1 2 3 4 5 6 7 8 9 10; do fetch_all "$url" 2>"$tmp/fetch"; rc=$?; [ $rc = 0 ] && break; sleep 2; done
  on A "$pb preview stop $id --json" >/dev/null 2>&1
  [ $rc = 0 ] || { cat "$tmp/fetch" >&2; return 1; }
}
tunnel() { [ -n "$tunnel" ] || { echo 'CANARY_TUNNEL_URL is not set' >&2; return 1; }; fetch_all "$tunnel"; }
edges() { # fetch_all only sees the addresses in DNS; a dead edge leaves DNS within its TTL and would pass
  local host=${tunnel#*://} ip bad=; host=${host%%/*}; host=${host%%:*}
  for ip in ${CANARY_EDGES:?}; do
    curl -fsS --max-time 10 --resolve "$host:443:$ip" "$tunnel/" >"$tmp/got" 2>"$tmp/curl" && want 2>>"$tmp/curl" ||
      bad="$bad edge $ip: $(tail -c 120 "$tmp/curl" | tr '\n' ' ')"
  done
  [ -z "$bad" ] || { echo "$bad" >&2; return 1; }
}

step origin origin
step exec_a_to_b exec_ab
step exec_b_to_a exec_ba
step ssh_a_to_b ssh_ab
step send_a_to_b send_ab
step preview preview
step tunnel tunnel
[ -z "${CANARY_EDGES:-}" ] || step edges edges
exit $failed
