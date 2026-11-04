#!/bin/sh
# Post-deploy edge gate (run after every server or edge deploy; deploy/README.md step 6).
# Fails when an edge cannot use the control API or a preview cannot complete TLS.
# Usage: check-edges.sh <preview-hostname> <ssh-target>=<public-ip> [...]
#   ssh-target "local" reads this host's edge container.
set -eu
[ $# -ge 2 ] || { echo "usage: $0 <preview-hostname> <ssh-target>=<public-ip> ..." >&2; exit 2; }
host=$1; shift
fail=0
for edge in "$@"; do
	target=${edge%%=*} ip=${edge#*=}
	logs='c=$(sudo docker ps --format "{{.Names}}" | grep -m1 -- "-tunnel$"); sudo docker logs --since 90s "$c" 2>&1 | grep -c -E "tunnel_control_request_failed|tunnel_route_reconcile_failed" || true'
	if [ "$target" = local ]; then n=$(sh -c "$logs"); else n=$(ssh "$target" "$logs"); fi
	# Any HTTP answer proves the edge completed TLS for the platform wildcard; 000 is a dropped handshake.
	code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 --resolve "$host:443:$ip" "https://$host/" || true)
	echo "edge $target ($ip): control failures in 90 s=$n, preview https=$code"
	if [ "$n" != 0 ] || [ "$code" = 000 ]; then fail=1; fi
done
[ $fail = 0 ] && echo "edge gate: PASS" || { echo "edge gate: FAIL (roll back: deploy/README.md step 5)"; exit 1; }
