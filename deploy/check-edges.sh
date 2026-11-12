#!/bin/sh
# Deploy gate (deploy/README.md steps 3, 4 and 6). It wraps a server or edge deploy and fails on anything a
# user would have seen during it. Every count below must be 0, over the whole window: setup, the deploy,
# and SETTLE seconds after it.
#   preview   non-200 answers for a preview that existed before the deploy, fetched through every edge
#             once a second (the request rides the probe machine's held carrier on that edge)
#   control   edge control failures (tunnel_control_request_failed, tunnel_route_reconcile_failed)
#   carrier   refused host carriers (carrier_connection_failed: data-carrier TLS handshake failures)
#   attach    host route attach refusals (runtime_route_attach_refused)
#   edge5xx   5xx pages the edge answered itself, dispatch failures included (edge_http_5xx)
#   server5xx support_reports rows with HTTP status >= 500 (every server component)
#   offline   machines online before the deploy that went offline during it
# Edge logs are followed from every tunnel container that runs in the window, the replaced one included.
# Usage: PROBE='<runs a shell on the paired probe machine>' SQL='<runs read-only psql on the production
#   database, SQL on stdin, -At output>' check-edges.sh [--deploy '<deploy command>'] <ssh-target>=<ip> ...
#   ssh-target "local" reads this host's edge. Without --deploy the gate watches for OBSERVE seconds.
set -eu
deploy=
if [ "${1:-}" = --deploy ]; then deploy=$2; shift 2; fi
[ $# -ge 1 ] && [ -n "${PROBE:-}" ] && [ -n "${SQL:-}" ] || {
	echo "usage: PROBE='<cmd>' SQL='<cmd>' $0 [--deploy '<cmd>'] <ssh-target>=<public-ip> ..." >&2
	exit 2
}
SETTLE=${SETTLE:-30} OBSERVE=${OBSERVE:-30} INTERVAL=${INTERVAL:-1}
# Scripts go over stdin, so ssh and docker exec -i never re-split them.
probe() { printf '%s\n' "$1" | $PROBE sh; }
sql() { printf 'set default_transaction_read_only = on;\n%s\n' "$1" | $SQL | grep -v '^SET$' || true; }
on() { if [ "$1" = local ]; then printf '%s\n' "$2" | sh; else printf '%s\n' "$2" | ssh "$1" sh; fi; }
work=$(mktemp -d)
start=$(date -u +%Y-%m-%dT%H:%M:%SZ)
stopfile=/tmp/pb-gate-$(date +%s)-$$.stop
port=18$(date +%S)7
cleanup() {
	touch "$work/stop"
	for edge in "$@"; do on "${edge%%=*}" "touch $stopfile" >/dev/null 2>&1 || true; done
	wait || true
	[ -n "${id:-}" ] && probe "pb preview stop $id >/dev/null 2>&1" || true
	probe 'kill $(cat /tmp/pb-gate-origin.pid) 2>/dev/null; rm -f /tmp/pb-gate-origin.pid' || true
	for edge in "$@"; do on "${edge%%=*}" "rm -f $stopfile" >/dev/null 2>&1 || true; done
	rm -rf "$work"
}
trap 'cleanup "$@"' EXIT
trap 'exit 1' INT TERM

# Follow each tunnel container's log from the window start until the gate stops; a replaced container's
# follower ends by itself when that container exits, after its last line. The followers run as root, so the
# script's own kill cannot reach them: each runs under `timeout`, and the exit trap pkills them through sudo
# (a surviving follower held the ssh session open for 30 min on 2026-11-05).
for edge in "$@"; do
	target=${edge%%=*}
	on "$target" "since=$start; stop=$stopfile; seen=' '; end=\$((\$(date +%s) + 3600))
trap 'sudo -n pkill -f \"[d]ocker logs --follow --since \$since\" 2>/dev/null' EXIT
while [ ! -e \$stop ] && [ \$(date +%s) -lt \$end ]; do
	for c in \$(sudo -n docker ps -q --no-trunc --filter name=-tunnel); do
		case \"\$seen\" in *\" \$c \"*) ;; *) seen=\"\$seen\$c \"; sudo -n timeout 3700 docker logs --follow --since \$since \$c 2>&1 & ;; esac
	done
	sleep 1
done
sleep 2" >"$work/edge-$target.log" 2>&1 &
done

# The preview is published only once the origin accepts a connection (10 s bound): publishing first lost the race.
probe "nohup python3 -m http.server $port --bind 127.0.0.1 >/dev/null 2>&1 & echo \$! > /tmp/pb-gate-origin.pid
for _ in \$(seq 50); do
	python3 -c \"import socket; socket.create_connection(('127.0.0.1', $port), 1).close()\" 2>/dev/null && exit 0
	sleep 0.2
done
exit 1" || { echo "edge gate: FAIL (the probe origin never listened on port $port)"; exit 1; }
json=$(probe "pb preview $port --background --ttl 30m --json")
id=$(printf '%s' "$json" | python3 -c 'import json,sys; d=json.load(sys.stdin); d=d.get("data",d); print(d["id"])')
url=$(printf '%s' "$json" | python3 -c 'import json,sys; d=json.load(sys.stdin); d=d.get("data",d); print(d.get("url") or d["endpoint"])')
host=${url#https://}; host=${host%%/*}
# Prints "<status> <seconds>"; the status alone is what the checks compare.
get() { curl -s -o /dev/null -w '%{http_code} %{time_total}' --max-time 5 --resolve "$host:443:$1" "https://$host/" || true; }
fail=0
for edge in "$@"; do # the new route reaches each edge within a few seconds
	code=000
	for _ in 1 2 3 4 5 6; do code=$(get "${edge#*=}"); code=${code%% *}; code=${code:-000}; [ "$code" = 200 ] && break; sleep 5; done
	echo "edge ${edge%%=*}: preview before the deploy https=$code"
	[ "$code" = 200 ] || fail=1
done
[ $fail = 0 ] || { echo "edge gate: FAIL (the preview never served; nothing deployed)"; exit 1; }

sql "select id from paperboat.user_machines where state = 'online' and online and deleted_at is null and revoked_at is null;" >"$work/online-before"
[ -s "$work/online-before" ] || { echo "edge gate: FAIL (could not read the machines online before the deploy)"; exit 1; }
: >"$work/probe-failed"; : >"$work/offline"
(
	n=0
	while [ ! -e "$work/stop" ]; do
		for edge in "$@"; do
			at=$(date -u +%T.%N | cut -c1-12)
			got=$(get "${edge#*=}"); code=${got%% *}; code=${code:-000}
			[ "$code" = 200 ] || echo "$at ${edge%%=*} $code ${got#* }s GET $host/" >>"$work/probe-failed"
		done
		n=$((n + 1))
		if [ $((n % 5)) = 0 ]; then
			sql "select id from paperboat.user_machines where updated_at >= '$start' and not online;" |
				grep -Fxf "$work/online-before" >>"$work/offline" || true
		fi
		sleep "$INTERVAL"
	done
) &

status=0
if [ -n "$deploy" ]; then
	sh -c "$deploy" || status=$?
	echo "deploy command exit=$status"
	sleep "$SETTLE"
else
	sleep "$OBSERVE"
fi
sql "select id from paperboat.user_machines where updated_at >= '$start' and not online;" | grep -Fxf "$work/online-before" >>"$work/offline" || true
touch "$work/stop"
for edge in "$@"; do on "${edge%%=*}" "touch $stopfile" >/dev/null 2>&1 || true; done
wait

[ "$status" = 0 ] || fail=1
count() { grep -c -E "$1" "$2" || true; }
for edge in "$@"; do
	target=${edge%%=*} log=$work/edge-${edge%%=*}.log
	probes=$(grep -c " $target " "$work/probe-failed" || true)
	control=$(count 'tunnel_control_request_failed|tunnel_route_reconcile_failed' "$log")
	carrier=$(count 'carrier_connection_failed' "$log")
	attach=$(count 'runtime_route_attach_refused' "$log")
	edge5xx=$(count 'edge_http_5xx' "$log")
	echo "edge $target: preview failures=$probes control=$control carrier=$carrier attach=$attach edge5xx=$edge5xx"
	grep -h 'carrier_connection_failed' "$log" | grep -o '"reason":"[a-z_]*"' | sort | uniq -c | sed 's/^/  carrier refusals: /' || true
	[ "$probes$control$carrier$attach$edge5xx" = 00000 ] || fail=1
done
# Edge error pages (a visitor's app refused/timed out) are counted from the edge logs above, not as server failures.
server5xx=$(sql "select count(*) from paperboat.support_reports where created_at >= '$start' and http_status >= 500 and code <> 'edge_error_page';")
offline=$(sort -u "$work/offline" | grep -c . || true)
echo "server: 5xx=$server5xx; machines online before and offline during the deploy=$offline"
sort -u "$work/offline" | sed 's/^/  offline: /'
[ "$server5xx" = 0 ] && [ "$offline" = 0 ] || fail=1
sed 's/^/  failed request: /' "$work/probe-failed"
if [ $fail = 0 ] && [ -z "${KEEP_LOGS:-}" ]; then echo "edge gate: PASS"; exit 0; fi
# The replaced container is removed with its logs, so the edge logs of the window are kept for the post-mortem.
keepdir=${KEEP_DIR:-/tmp}
kept=$(mktemp -d "$keepdir"/pb-gate-failed.XXXXXX); cp "$work"/edge-*.log "$kept"/
echo "edge logs of the window kept in $kept"
# Only the newest KEEP_LAST (default 5) kept directories stay in KEEP_DIR (default /tmp); each deploy would otherwise add one for good.
ls -dt "$keepdir"/pb-gate-failed.* | tail -n +$(( ${KEEP_LAST:-5} + 1 )) | while read -r old; do rm -rf -- "$old"; done
[ $fail = 0 ] && { echo "edge gate: PASS"; exit 0; }
echo "edge gate: FAIL (roll back: deploy/README.md step 5)"; exit 1
