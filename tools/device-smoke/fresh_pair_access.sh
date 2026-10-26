#!/bin/bash
# fresh_pair_access.sh HOST BINARY
#
# Pairs two FRESH throwaway containers on HOST (a Linux VM with Docker and the laneh-ubuntu24-systemd image, whose
# user `pb` is signed in to the test account and only mints enrollment tokens) with BINARY, then runs
# `pb access machine <target> --port N` from the client container to a marker origin on the target, end to end,
# with no daemon restart in between. Machines paired before a
# regression never show a missing local-address lease (the private-access P0 of 2026-10-26); only a fresh pair
# does. Prints one JSON line {"check":"fresh-pair-access","status":"PASS"|"FAIL",...}; the machine is revoked by
# exact id and the container removed on every exit path. The enrollment token stays in files.
set -uo pipefail
HOST=${1:?host} BIN=${2:?binary}
HERE=$(cd "$(dirname "$0")" && pwd)
STAMP=$(date +%s)
TARGET=fresh-access-t-$STAMP CLIENT=fresh-access-c-$STAMP ALIAS=lanea-fpa-$STAMP PORT=18473 LISTEN=127.0.0.1:18474
MACHINE_IDS=

finish() {
  local status=$1 detail=$2 id
  ssh "$HOST" "sudo -n docker rm -f $TARGET $CLIENT >/dev/null 2>&1; rm -f /tmp/$TARGET-pb" || true
  for id in $MACHINE_IDS; do
    python3 "$HERE/cleanup_test_account.py" --id "$id" --stale '0 seconds' --apply >/dev/null 2>&1 || detail="$detail; revoke of $id failed"
  done
  python3 -c 'import json,sys;print(json.dumps({"check":"fresh-pair-access","status":sys.argv[1],"machine_ids":sys.argv[2].split(),"detail":sys.argv[3]}))' "$status" "$MACHINE_IDS" "$detail"
  [ "$status" = PASS ]
  exit $?
}

scp -q "$BIN" "$HOST:/tmp/$TARGET-pb" || finish FAIL "could not copy the build to $HOST"
out=$( { echo "FRESH_BINARY=/tmp/$TARGET-pb"; cat "$HERE/fresh_pair_lib.sh"; cat <<EOF; } | ssh "$HOST" "bash -s" 2>&1
set -euo pipefail
fresh $TARGET $ALIAS
fresh $CLIENT $ALIAS-c
sudo -n docker exec -u pbt -e HOME=/home/pbt $TARGET bash -c 'mkdir -p ~/origin && echo FRESH_ACCESS_$STAMP > ~/origin/marker && cd ~/origin && nohup python3 -m http.server $PORT --bind 127.0.0.1 >/dev/null 2>&1 &'
C="sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 $CLIENT"
\$C bash -c 'timeout 150 ~/.local/bin/pb access machine $ALIAS --port $PORT --listen $LISTEN > /tmp/access.log 2>&1 &'
for i in \$(seq 1 120); do
  if \$C curl -fsS --max-time 5 http://$LISTEN/marker 2>/dev/null | grep -qx FRESH_ACCESS_$STAMP; then echo ACCESS_OK; break; fi
  \$C pgrep -f "pb access machine" >/dev/null || { echo ACCESS_EXITED; \$C tail -c 600 /tmp/access.log; break; }
  sleep 1
done
EOF
)
MACHINE_IDS=$(sed -n 's/^MACHINE_ID=\(machine_[0-9a-f-]*\)$/\1/p' <<<"$out" | tr '\n' ' ')
if grep -qx ACCESS_OK <<<"$out"; then
  finish PASS "two fresh pairs, then pb access machine from one reached the other's origin with no daemon restart"
fi
finish FAIL "$(tail -c 400 <<<"$out" | tr '\n' ' ')"
