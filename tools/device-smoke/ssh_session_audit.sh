#!/bin/bash
# ssh_session_audit.sh HOST BINARY  (check id: ssh-session-audit, CHECKLIST F5-12)
#
# Each managed SSH session's start and end reach the workspace audit log. Pairs two FRESH throwaway containers
# on HOST (a Linux VM with Docker and the laneh-ubuntu24-systemd image; see fresh_pair_lib.sh) with BINARY: the
# target runs OpenSSH (installed BEFORE pairing so the setup user is registered), the client runs one real
# `pb ssh <target> -- echo <marker>`. The target host queues machine.access_started and machine.access_ended
# (capability managed_ssh, origin online, with the client's account and endpoint, the generation, and the bytes
# each way) and uploads them within its 30 s cadence; the check reads them from paperboat.audit_events (read-only,
# the operator access cleanup_test_account.py uses) and requires: a start and an end for this machine, the end
# naming the start's session id with outcome closed and bytes_in/bytes_out > 0, both with origin online.
# Prints one JSON line {"check":"ssh-session-audit","status":"PASS"|"FAIL",...}. Machines are revoked by exact id and
# containers removed on every exit path; the enrollment token stays in files.
set -uo pipefail
HOST=${1:?host} BIN=${2:?binary}
DB_HOST=${PB_AUDIT_DB_HOST:-root@coolify}
HERE=$(cd "$(dirname "$0")" && pwd)
STAMP=$(date +%s)
TARGET=lanes-sa-t-$STAMP CLIENT=lanes-sa-c-$STAMP ALIAS=lanes-sa-$STAMP
MACHINE_IDS= TARGET_ID=

finish() {
  local status=$1 detail=$2 id
  ssh "$HOST" "sudo -n docker rm -f $TARGET $CLIENT >/dev/null 2>&1; rm -f /tmp/$TARGET-pb" || true
  for id in $MACHINE_IDS; do
    python3 "$HERE/cleanup_test_account.py" --id "$id" --stale '0 seconds' --apply >/dev/null 2>&1 || detail="$detail; revoke of $id failed"
  done
  python3 -c 'import json,sys;print(json.dumps({"check":"ssh-session-audit","status":sys.argv[1],"machine_ids":sys.argv[2].split(),"detail":sys.argv[3]}))' "$status" "$MACHINE_IDS" "$detail"
  [ "$status" = PASS ]
  exit $?
}

scp -q "$BIN" "$HOST:/tmp/$TARGET-pb" || finish FAIL "could not copy the build to $HOST"
out=$( { echo "FRESH_BINARY=/tmp/$TARGET-pb"; cat "$HERE/fresh_pair_lib.sh"; cat <<EOF; } | ssh "$HOST" "bash -s" 2>&1
set -euo pipefail
start_ct $TARGET
sudo -n docker exec $TARGET bash -c "DEBIAN_FRONTEND=noninteractive apt-get -qq update >/dev/null 2>&1; DEBIAN_FRONTEND=noninteractive apt-get -qq install -y openssh-server >/dev/null 2>&1; true"
# the image may carry sshd host keys another container already published (cloned image): make this target's own
sudo -n docker exec $TARGET bash -c "rm -f /etc/ssh/ssh_host_*; ssh-keygen -A >/dev/null; systemctl restart ssh"
pair_user $TARGET pbt 1000 $ALIAS
fresh $CLIENT $ALIAS-c
C="sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 $CLIENT"
for i in \$(seq 1 60); do \$C bash -c '~/.local/bin/pb machine list 2>/dev/null' | grep -q "$ALIAS " && break; sleep 2; done
for i in \$(seq 1 30); do
  got=\$(\$C bash -c '~/.local/bin/pb ssh $ALIAS -- echo ssh-audit-$STAMP' </dev/null 2>&1 || true)
  if grep -q "ssh-audit-$STAMP" <<<"\$got"; then echo SSH_OK; break; fi
  sleep 4
done
grep -q "ssh-audit-$STAMP" <<<"\${got:-}" || echo "SSH_OUT=\$(tail -c 300 <<<"\${got:-}" | tr '\n' ' ')"
EOF
)
# the paired machines, target first
MACHINE_IDS=$(sed -n 's/^MACHINE_ID=\(machine_[0-9a-f-]*\)$/\1/p' <<<"$out" | tr '\n' ' ')
TARGET_ID=${MACHINE_IDS%% *}
[ -n "$TARGET_ID" ] || finish FAIL "pairing failed: $(tail -c 300 <<<"$out" | tr '\n' ' ')"
grep -qx SSH_OK <<<"$out" || finish FAIL "pb ssh did not run: $(grep '^SSH_OUT=' <<<"$out" | head -1)"

# The host uploads within its 30 s cadence; read the target's events.
query="SELECT event_type, metadata->>'origin', metadata->>'session_id', metadata->>'outcome', coalesce(metadata->>'bytes_in','0'), coalesce(metadata->>'bytes_out','0'), metadata->>'peer_endpoint_id', idempotency_key FROM paperboat.audit_events WHERE resource_id='$TARGET_ID' AND event_type IN ('machine.access_started','machine.access_ended') AND metadata->>'capability'='managed_ssh' ORDER BY created_at, id;"
rows=
for i in $(seq 1 30); do
  rows=$(printf '%s\n' "$query" | ssh "$DB_HOST" docker exec -i paperboat-postgres-1 psql -U postgres -d paperboat -qAt -F "'|'" 2>/dev/null)
  grep -q '^machine.access_ended|' <<<"$rows" && break
  sleep 5
done
python3 - "$rows" <<'PY' && finish PASS "start and end audited: $(tr '\n' ' ' <<<"$rows" | cut -c1-300)" || finish FAIL "audit events wrong or missing: $(tr '\n' ' ' <<<"$rows" | cut -c1-400)"
import sys
rows = [r.split("|") for r in sys.argv[1].splitlines() if r]
starts = [r for r in rows if r[0] == "machine.access_started"]
ends = [r for r in rows if r[0] == "machine.access_ended"]
# An end names its start: the start's host event id, which the server files as the idempotency key "offline:<id>".
start_ids = {s[7].removeprefix("offline:") for s in starts}
ok = bool(starts) and bool(ends)
for e in ends:
    ok = ok and e[1] == "online" and e[3] == "closed" and int(e[4]) > 0 and int(e[5]) > 0 and e[2] in start_ids
for s in starts:
    ok = ok and s[1] == "online" and s[6].startswith("session_")
sys.exit(0 if ok else 1)
PY
