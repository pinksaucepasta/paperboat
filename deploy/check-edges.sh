#!/bin/sh
# Post-deploy edge gate (run after every server or edge deploy; deploy/README.md step 6).
# Fails when an edge cannot use the control API or a fresh preview cannot complete TLS through it.
# The gate makes its own preview on a paired probe machine, GETs it through every edge, then stops it.
# Usage: PROBE='<command prefix that runs a shell on the probe>' check-edges.sh <ssh-target>=<public-ip> ...
#   e.g. PROBE='ssh pbvm2 sudo docker exec -i -u pbt -e HOME=/home/pbt -e PATH=/home/pbt/.local/bin:/usr/bin:/bin pb-probe'; ssh-target "local" reads this host's edge container.
set -eu
[ $# -ge 1 ] && [ -n "${PROBE:-}" ] || { echo "usage: PROBE='<cmd>' $0 <ssh-target>=<public-ip> ..." >&2; exit 2; }
# The script goes over stdin, so ssh and docker exec -i never re-split it.
probe() { printf '%s\n' "$1" | $PROBE sh; }
port=18$(date +%S)7
probe "nohup python3 -m http.server $port --bind 127.0.0.1 >/dev/null 2>&1 & echo \$! > /tmp/pb-gate-origin.pid"
cleanup() {
	[ -n "${id:-}" ] && probe "pb preview stop $id >/dev/null 2>&1" || true
	probe 'kill $(cat /tmp/pb-gate-origin.pid) 2>/dev/null; rm -f /tmp/pb-gate-origin.pid' || true
}
trap cleanup EXIT
json=$(probe "pb preview $port --background --ttl 10m --json")
id=$(printf '%s' "$json" | python3 -c 'import json,sys; d=json.load(sys.stdin); d=d.get("data",d); print(d["id"])')
url=$(printf '%s' "$json" | python3 -c 'import json,sys; d=json.load(sys.stdin); d=d.get("data",d); print(d.get("url") or d["endpoint"])')
host=${url#https://}; host=${host%%/*}
fail=0
for edge in "$@"; do
	target=${edge%%=*} ip=${edge#*=}
	logs='c=$(sudo docker ps --format "{{.Names}}" | grep -m1 -- "-tunnel$"); sudo docker logs --since 90s "$c" 2>&1 | grep -c -E "tunnel_control_request_failed|tunnel_route_reconcile_failed" || true'
	if [ "$target" = local ]; then n=$(sh -c "$logs"); else n=$(ssh "$target" "$logs"); fi
	code=000
	for _ in 1 2 3 4 5 6; do # the new route reaches each edge within a few seconds
		code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 --resolve "$host:443:$ip" "https://$host/" || true)
		[ "$code" = 200 ] && break
		sleep 5
	done
	echo "edge $target ($ip): control failures in 90 s=$n, fresh preview https=$code"
	if [ "$n" != 0 ] || [ "$code" != 200 ]; then fail=1; fi
done
[ $fail = 0 ] && echo "edge gate: PASS" || { echo "edge gate: FAIL (roll back: deploy/README.md step 5)"; exit 1; }
