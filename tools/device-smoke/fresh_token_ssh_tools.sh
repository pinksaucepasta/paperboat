#!/bin/bash
# fresh_token_ssh_tools.sh HOST BINARY
#
# Two FRESH throwaway containers on HOST (as fresh_pair_access.sh: user `pb` there only mints enrollment tokens) are
# enrolled from tokens, no login, and the client has no ssh key or agent. From the client `pb ssh`, `pb scp` and
# `pb rsync` must reach the target by alias, by name and by machine id (fresh_token_ssh_tools_client.sh); scp and
# rsync once refused machine_<id> operands (b0f14df28) and nothing but a fresh token-only client would have seen
# the missing key/agent. Prints one JSON line {"check":"fresh-token-ssh-tools","status":...}; machines are revoked
# by exact id and containers removed on every exit path.
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/ct_env.sh"
set -uo pipefail
. "$(cd "$(dirname "$0")" && pwd)/stage_binary.sh"
HOST=${1:?host} BIN=${2:?binary}
HERE=$(cd "$(dirname "$0")" && pwd)
STAMP=$(date +%s)
TARGET=fresh-tools-t-$STAMP CLIENT=fresh-tools-c-$STAMP ALIAS=test-fts-$STAMP
MACHINE_IDS=

finish() {
  local status=$1 detail=$2 id
  ssh "$HOST" "$CT_DOCKER rm -f $TARGET $CLIENT >/dev/null 2>&1; rm -f /tmp/$TARGET-pb /tmp/$TARGET-client.sh" || true
  for id in $MACHINE_IDS; do
    python3 "$HERE/cleanup_test_account.py" --id "$id" --stale '0 seconds' --apply >/dev/null 2>&1 || detail="$detail; revoke of $id failed"
  done
  python3 -c 'import json,sys;print(json.dumps({"check":"fresh-token-ssh-tools","status":sys.argv[1],"machine_ids":sys.argv[2].split(),"detail":sys.argv[3]}))' "$status" "$MACHINE_IDS" "$detail"
  [ "$status" = PASS ]
  exit $?
}

stage_binary "$BIN" "$HOST" "/tmp/$TARGET-pb" || finish FAIL "could not copy the build to $HOST"
scp -q "$HERE/fresh_token_ssh_tools_client.sh" "$HOST:/tmp/$TARGET-client.sh" || finish FAIL "could not copy the client script to $HOST"
out=$( { echo "FRESH_BINARY=/tmp/$TARGET-pb"; ct_env_remote; cat "$HERE/fresh_pair_lib.sh"; cat <<EOF; } | ssh "$HOST" "bash -s" 2>&1
set -euo pipefail
fresh $TARGET $ALIAS
fresh $CLIENT $ALIAS-c
for c in $TARGET $CLIENT; do $CT_DOCKER exec -e DEBIAN_FRONTEND=noninteractive \$c bash -c 'apt-get -qq install -y rsync >/dev/null 2>&1'; done
$CT_DOCKER cp /tmp/$TARGET-client.sh $CLIENT:$CT_HOME/client.sh
$CT_DOCKER exec $CLIENT chown $CT_USER:$CT_USER $CT_HOME/client.sh
TID=\$($CT_DOCKER exec -u $CT_USER -e HOME=$CT_HOME $TARGET python3 -c 'import json;print(json.load(open("$CT_HOME/.local/state/paperboat/runtime/machine-registration.json"))["machine_id"])')
$CT_DOCKER exec -u $CT_USER -e HOME=$CT_HOME -e XDG_RUNTIME_DIR=$CT_RUNDIR -e ALIAS=$ALIAS -e TARGET_ID=\$TID -e STAMP=$STAMP $CLIENT bash $CT_HOME/client.sh || echo CLIENT_FAILED
EOF
)
MACHINE_IDS=$(sed -n 's/^MACHINE_ID=\(machine_[0-9a-f-]*\)$/\1/p' <<<"$out" | tr '\n' ' ')
results=$(grep '^RESULT ' <<<"$out")
passes=$(grep -c '^RESULT PASS ' <<<"$results")
# 3 references x (ssh + scp + rsync): nothing counts as a pass unless every one reported PASS.
if [ "$passes" = 9 ] && ! grep -q 'CLIENT_FAILED\|^RESULT FAIL' <<<"$out"; then
  finish PASS "ssh, scp and rsync by alias, name and machine id from a token-only client with no ssh key or agent: 9/9, hashes verified on the target"
fi
finish FAIL "$(grep -E '^RESULT FAIL' <<<"$out" | head -4 | tr '\n' ' ') ${passes}/9 passed; $(tail -c 200 <<<"$out" | tr '\n' ' ')"
