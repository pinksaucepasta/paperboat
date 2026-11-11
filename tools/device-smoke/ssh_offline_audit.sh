#!/bin/bash
# ssh_offline_audit.sh HOST BINARY  (check id: ssh-offline-audit, CHECKLIST F5-12 offline half)
#
# Managed SSH sessions started while Paperboat was unreachable appear in the workspace audit log with
# origin 'offline' once the control plane is reachable again. Pairs two FRESH throwaway containers on HOST
# (a Linux VM with Docker and the laneh-ubuntu24-systemd image; see fresh_pair_lib.sh) with BINARY: the target
# runs OpenSSH (installed BEFORE pairing so the setup user is registered), the client runs one real
# `pb ssh <target> -- echo <marker>` while the control plane is blackholed in BOTH containers. The host records
# origin offline only when the client sent the offline header (pb ssh always does: localdaemon/source.go
# hostRulesPeerStream) AND its signed network config is past its refresh hint (tailnet/authority.go OutOfSync:
# now >= current.ExpiresAt, server/internal/peersessions/network.go:459 admission.Expiry = cli_auth
# mint_proof_lifetime, 2 min by default). The outage is therefore 150 s (> 120 s hint); the 45 s outage of
# ssh_control_outage.sh is too short for 'offline'. After the routes are removed the host uploads within its
# 30 s cadence; the check reads paperboat.audit_events (read-only, the operator access cleanup_test_account.py
# uses) and requires: an offline-origin start and its end (same session id, outcome closed, bytes_in/out > 0).
# Prints one JSON line {"check":"ssh-offline-audit","status":"PASS"|"FAIL",...}. Machines are revoked by exact id,
# containers removed and blackhole routes removed on every exit path; the enrollment token stays in files.
set -uo pipefail
HOST=${1:?host} BIN=${2:?binary}
DB_HOST=${PB_AUDIT_DB_HOST:-root@coolify}
HERE=$(cd "$(dirname "$0")" && pwd)
STAMP=$(date +%s)
TARGET=laneh-soa-t-$STAMP CLIENT=laneh-soa-c-$STAMP ALIAS=laneh-soa-$STAMP
MACHINE_IDS= TARGET_ID= ADDRS=
OUTAGE_SECS=150

finish() {
  local status=$1 detail=$2 id a ct
  # routes first: the outage must not survive this check on any exit path
  if [ -n "$ADDRS" ]; then
    for ct in $TARGET $CLIENT; do
      for a in $ADDRS; do
        ssh "$HOST" "sudo -n docker exec $ct ip route del blackhole $a >/dev/null 2>&1 || sudo -n docker exec $ct ip -6 route del blackhole $a >/dev/null 2>&1" || true
      done
    done
  fi
  ssh "$HOST" "sudo -n docker rm -f $TARGET $CLIENT >/dev/null 2>&1; rm -f /tmp/$TARGET-pb" || true
  for id in $MACHINE_IDS; do
    python3 "$HERE/cleanup_test_account.py" --id "$id" --stale '0 seconds' --apply >/dev/null 2>&1 || detail="$detail; revoke of $id failed"
  done
  python3 -c 'import json,sys;print(json.dumps({"check":"ssh-offline-audit","status":sys.argv[1],"machine_ids":sys.argv[2].split(),"detail":sys.argv[3]}))' "$status" "$MACHINE_IDS" "$detail"
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
  got=\$(\$C bash -c '~/.local/bin/pb ssh $ALIAS -- echo ssh-offline-baseline-$STAMP' </dev/null 2>&1 || true)
  if grep -q "ssh-offline-baseline-$STAMP" <<<"\$got"; then echo SSH_OK; break; fi
  sleep 4
done
grep -q "ssh-offline-baseline-$STAMP" <<<"\${got:-}" || echo "SSH_OUT=\$(tail -c 300 <<<"\${got:-}" | tr '\n' ' ')"
EOF
)
# the paired machines, target first
MACHINE_IDS=$(sed -n 's/^MACHINE_ID=\(machine_[0-9a-f-]*\)$/\1/p' <<<"$out" | tr '\n' ' ')
TARGET_ID=${MACHINE_IDS%% *}
[ -n "$TARGET_ID" ] || finish FAIL "pairing failed: $(tail -c 300 <<<"$out" | tr '\n' ' ')"
grep -qx SSH_OK <<<"$out" || finish FAIL "pb ssh did not run before the outage: $(grep '^SSH_OUT=' <<<"$out" | head -1)"

# the control plane's addresses, blackholed in target AND client (no server is touched)
api=$(ssh "$HOST" "sudo -n docker exec -u pbt $CLIENT sed -n 's/.*\"server_url\": *\"https:\\/\\/\\([^\"]*\\)\".*/\\1/p' /home/pbt/.config/paperboat/config.json" 2>/dev/null)
[ -n "$api" ] || finish FAIL "could not read server_url from the client container"
ADDRS=$(ssh "$HOST" "sudo -n docker exec $CLIENT getent ahosts $api 2>/dev/null | awk '{print \$1}' | sort -u" 2>/dev/null)
[ -n "$ADDRS" ] || finish FAIL "could not resolve $api in the client container"
for ct in $TARGET $CLIENT; do
  for a in $ADDRS; do
    ssh "$HOST" "sudo -n docker exec $ct ip route add blackhole $a 2>/dev/null || sudo -n docker exec $ct ip -6 route add blackhole $a 2>/dev/null" || finish FAIL "could not blackhole $api ($a) in $ct"
  done
done
# the outage must outlast the signed network's refresh hint (2 min) so the host's OutOfSync is true
blackholed_at=$(date +%s)
sleep "$OUTAGE_SECS"
# while still blackholed, one real ssh: the client always sends the offline header, the host records
# origin offline only past the hint
off_out=$(ssh "$HOST" "sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 $CLIENT timeout 60 bash -c '~/.local/bin/pb ssh $ALIAS -- echo ssh-offline-$STAMP' </dev/null 2>&1" || true)
grep -q "ssh-offline-$STAMP" <<<"$off_out" || { detail="pb ssh did not run during the outage after $OUTAGE_SECS s: $(tail -c 300 <<<"$off_out" | tr '\n' ' ')"; finish FAIL "$detail"; }
# routes removed again before the audit read (finish repeats this on every exit path)
for ct in $TARGET $CLIENT; do
  for a in $ADDRS; do
    ssh "$HOST" "sudo -n docker exec $ct ip route del blackhole $a >/dev/null 2>&1 || sudo -n docker exec $ct ip -6 route del blackhole $a >/dev/null 2>&1" || true
  done
done
ADDRS=
reconnected_at=$(date +%s)

# The host uploads within its 30 s cadence; read the target's offline-origin events.
query="SELECT event_type, metadata->>'origin', metadata->>'session_id', metadata->>'outcome', coalesce(metadata->>'bytes_in','0'), coalesce(metadata->>'bytes_out','0'), metadata->>'peer_endpoint_id', idempotency_key FROM paperboat.audit_events WHERE resource_id='$TARGET_ID' AND event_type IN ('machine.access_started','machine.access_ended') AND metadata->>'capability'='managed_ssh' ORDER BY created_at, id;"
rows=
for i in $(seq 1 30); do
  rows=$(printf '%s\n' "$query" | ssh "$DB_HOST" docker exec -i paperboat-postgres-1 psql -U postgres -d paperboat -qAt -F "'|'" 2>/dev/null)
  grep -q '|offline|' <<<"$rows" && break
  sleep 5
done
outage_wall=$(( reconnected_at - blackholed_at ))
python3 - "$rows" "$outage_wall" <<'PY' && finish PASS "offline start and end audited after ${outage_wall}s outage: $(tr '\n' ' ' <<<"$rows" | grep -o 'offline|[^ ]*' | head -2 | tr '\n' ' '; tr '\n' ' ' <<<"$rows" | cut -c1-250)" || finish FAIL "offline audit events wrong or missing after ${outage_wall}s outage: $(tr '\n' ' ' <<<"$rows" | cut -c1-400)"
import sys
rows = [r.split("|") for r in sys.argv[1].splitlines() if r]
starts = [r for r in rows if r[0] == "machine.access_started" and r[1] == "offline"]
ends = [r for r in rows if r[0] == "machine.access_ended" and r[1] == "offline"]
# An end names its start: the start's host event id, which the server files as the idempotency key "offline:<id>".
start_ids = {s[7].removeprefix("offline:") for s in starts}
ok = bool(starts) and bool(ends)
for e in ends:
    ok = ok and e[3] == "closed" and int(e[4]) > 0 and int(e[5]) > 0 and e[2] in start_ids
for s in starts:
    ok = ok and s[6].startswith("session_")
sys.exit(0 if ok else 1)
PY
