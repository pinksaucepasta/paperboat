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
#   CANARY_ENV_NAME/_VALUE  one managed ENV value set once for the pair's workspace (fixture); the env_exec step needs both
#   CANARY_TEAM / CANARY_TEAM_MACHINE  a team the canary account owns and a machine the team owns with an exact exec
#                        grant to that account (fixture); when set, team_exec runs A -> that machine in the team workspace
#   ssh_b_to_a send_b_to_a api_auth_b  (opt-in) the same actions driven from B, for the vantage point that hosts B (tools/probe/status_run.py)
#   CANARY_EDGES         every managed edge address (space separated); each must serve the canary tunnel
#                        directly, so an edge that dropped out of DNS or was null-routed still fails a step.
#                        The preview step also fetches that run's own new preview through each address and reports
#                        one step per address, edge_<address> (a fixed preview URL would make one lost preview read as every edge down)
#   PREVIEW_ON           A (default) or B: the machine that makes the preview (database test host's vantage point has only B); each preview gets its own short-lived origin
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
  local rc=0
  "$@" >"$tmp/out" 2>"$tmp/err" || rc=$?
  if [ $rc = 0 ]; then
    printf '{"step":"%s","ok":true,"seconds":%d}\n' "$name" $((SECONDS - start))
  else
    failed=1
    local why; why=$(tail -c 300 "$tmp/err"); [ -n "$why" ] || why="exit $rc with no output (a killed command, or a silent failure)"
    printf '{"step":"%s","ok":false,"seconds":%d,"error":%s}\n' "$name" $((SECONDS - start)) \
      "$(printf '%s' "$why" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read().strip()))')"
  fi
}
want() { grep -qxF "$marker" "$tmp/got" || { echo "expected the run marker, got: $(head -c 120 "$tmp/got")" >&2; return 1; }; }
fetch_via() { # ip url: this one edge address must serve the run marker
  local host=${2#*://} port=443; host=${host%%/*}; [[ $host != *:* ]] || port=${host##*:}; host=${host%%:*}
  curl -fsS --max-time 10 --resolve "$host:$port:$1" "$2/" >"$tmp/got" 2>"$tmp/curl" && want 2>>"$tmp/curl" ||
    { echo "edge $1: $(tail -c 120 "$tmp/curl" | tr '\n' ' ')" >&2; return 1; }
}
fetch_all() { # url: every edge address in DNS must serve the run marker (DNS alone would test one at random)
  local host=${1#*://} ip bad=; host=${host%%/*}; host=${host%%:*}
  local ips; ips=$(getent ahostsv4 "$host" | awk '{print $1}' | sort -u)
  [ -n "$ips" ] || { echo "no address for $host" >&2; return 1; }
  for ip in $ips; do bad="$bad $(fetch_via "$ip" "$1" 2>&1)"; done
  [ -z "${bad// /}" ] || { echo "$bad" >&2; return 1; }
}

origin() { on A "mkdir -p canary-www && echo $marker >canary-www/index.html && curl -fsS --max-time 5 http://127.0.0.1:$port/" >"$tmp/got" && want; }
exec_ab() { on A "$pb exec $b -- echo $marker" >"$tmp/got" && want; }
exec_ba() { on B "$pb exec $a -- echo $marker" >"$tmp/got" && want; }
env_exec() { # A runs a command on B: the managed ENV value must arrive (never printed)
  [ -n "${CANARY_ENV_NAME:-}" ] && [ -n "${CANARY_ENV_VALUE:-}" ] || { echo "CANARY_ENV_NAME and CANARY_ENV_VALUE are not set" >&2; return 1; }
  on A "$pb exec $b -- printenv $CANARY_ENV_NAME" >"$tmp/got" 2>"$tmp/execerr" || { tail -c 200 "$tmp/execerr" >&2; return 1; }
  [ "$(cat "$tmp/got")" = "$CANARY_ENV_VALUE" ] || { echo "$CANARY_ENV_NAME did not arrive on $b" >&2; tail -c 200 "$tmp/execerr" >&2; return 1; }
}
team_exec() { on A "$pb --workspace ${CANARY_TEAM:?} exec $CANARY_TEAM_MACHINE -- echo $marker" >"$tmp/got" && want; }
ssh_ab() { on A "$pb ssh $b -- echo $marker" >"$tmp/got" && want; }
send_dir() { # FROM TO: FROM sends a file to machine TO, which finds it in its Inbox with the marker inside, then removes it
  local from=$1 to=$2 fromname=$3 toname=$4
  on "$from" "echo $marker >/tmp/$marker.txt && $pb send /tmp/$marker.txt --to $toname >/dev/null; rc=\$?; rm -f /tmp/$marker.txt; exit \$rc" &&
    on "$to" "i=\$($pb inbox path --json | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d.get(\"data\",d)[\"path\"])') || exit 1
for _ in \$(seq 20); do f=\$(find \"\$i\" -name '$marker.txt' -print -quit); [ -n \"\$f\" ] && break; sleep 1; done
[ -n \"\$f\" ] || { echo 'file not in the Inbox after 20 s' >&2; exit 1; }
cat \"\$f\"; rm -f \"\$f\"" >"$tmp/got" && want
}
send_ab() { send_dir A B "$a" "$b"; }
send_ba() { # B sends a file to A and A's daemon must acknowledge delivery (the host that runs B has no access to A's Inbox)
  on B "echo $marker >/tmp/$marker.txt && $pb send /tmp/$marker.txt --to $a; rc=\$?; rm -f /tmp/$marker.txt; exit \$rc" >"$tmp/got" 2>&1 && grep -q "delivered to" "$tmp/got" ||
    { echo "no delivery acknowledgement from $a: $(tail -c 150 "$tmp/got")" >&2; return 1; }
}
ssh_ba() { on B "$pb ssh $a -- echo $marker" >"$tmp/got" && want; }
api_auth() { # X peer: an authenticated read of the account's machines from machine X must list the other canary machine
  on "$1" "$pb machine list --json" >"$tmp/got" 2>"$tmp/execerr" || { tail -c 200 "$tmp/execerr" >&2; return 1; }
  grep -qF "$2" "$tmp/got" || { echo "machine list does not show $2" >&2; return 1; }
}
declare -A edge_err # edge address -> why it did not serve this run's preview ("" once it did); unset when no preview was made
preview() { # a new preview of its own origin, fetched from here without credentials (and through each edge address), then stopped
  local side=${PREVIEW_ON:-A} p=$((30000 + RANDOM % 20000)) # its own short-lived origin in a private directory: concurrent probes never share a marker file
  on "$side" "d=\$(mktemp -d) && echo $marker >\$d/index.html && (setsid sh -c \"timeout 90 python3 -m http.server $p --bind 127.0.0.1 --directory \$d; rm -rf \$d\" >/dev/null 2>&1 &)" || return 1
  on "$side" "$pb preview $p --background --ttl 2m --json" >"$tmp/preview.json" || return 1
  local id url
  id=$(python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));d=d.get("data",d);print(d["id"])' "$tmp/preview.json") &&
    url=$(python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));d=d.get("data",d);print(d.get("url") or d["endpoint"])' "$tmp/preview.json") ||
    { echo "unexpected preview answer: $(head -c 200 "$tmp/preview.json")" >&2; return 1; }
  local rc ip pending deadline=$((SECONDS + 20)) # a new route reaches the edges within seconds (check-edges.sh allows 30 s)
  for ip in ${CANARY_EDGES:-}; do edge_err[$ip]="not tried"; done
  while :; do
    fetch_all "$url" 2>"$tmp/fetch"; rc=$?; pending=
    for ip in ${CANARY_EDGES:-}; do
      [ -z "${edge_err[$ip]}" ] || edge_err[$ip]=$(fetch_via "$ip" "$url" 2>&1 || true)
      [ -z "${edge_err[$ip]}" ] || pending=1
    done
    { [ $rc = 0 ] && [ -z "$pending" ]; } || [ $SECONDS -ge $deadline ] && break
    sleep 2
  done
  on "$side" "$pb preview stop $id --json" >/dev/null 2>&1
  [ $rc = 0 ] || { cat "$tmp/fetch" >&2; return 1; }
}
edge_result() { [ -z "${edge_err[$1]}" ] || { echo "${edge_err[$1]}" >&2; return 1; }; }
tunnel() { [ -n "$tunnel" ] || { echo 'CANARY_TUNNEL_URL is not set' >&2; return 1; }; fetch_all "$tunnel"; }
edges() { # fetch_all only sees the addresses in DNS; a dead edge leaves DNS within its TTL and would pass
  local ip bad=
  for ip in ${CANARY_EDGES:?}; do bad="$bad $(fetch_via "$ip" "$tunnel" 2>&1)"; done
  [ -z "${bad// /}" ] || { echo "$bad" >&2; return 1; }
}

step origin origin
step exec_a_to_b exec_ab
step exec_b_to_a exec_ba
step ssh_a_to_b ssh_ab
step env_exec env_exec
[ -z "${CANARY_TEAM_MACHINE:-}" ] || step team_exec team_exec
step api_auth_a api_auth A "$b"
step send_a_to_b send_ab
step preview preview
for ip in ${CANARY_EDGES:-}; do [ -z "${edge_err[$ip]+set}" ] || step "edge_$ip" edge_result "$ip"; done
step tunnel tunnel
[ -z "${CANARY_EDGES:-}" ] || step edges edges
# Steps driven from machine B for a second vantage point (the host that runs B): run only when PROBE_STEPS names them.
optional() { [[ " ${PROBE_STEPS:-} " == *" $1 "* ]] && step "$@"; return 0; }
optional ssh_b_to_a ssh_ba
optional send_b_to_a send_ba
optional api_auth_b api_auth B "$a"
exit $failed
