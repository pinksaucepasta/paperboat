#!/bin/bash
# ssh_offline_audit.sh HOST BINARY  (check id: ssh-offline-audit, CHECKLIST F5-12 offline half)
#
# Managed SSH sessions started while Paperboat was unreachable appear in the workspace audit log with
# origin 'offline' once the control plane is reachable again. Pairs two FRESH throwaway containers on HOST
# (a Linux VM with Docker and the laneh-ubuntu24-systemd image; see fresh_pair_lib.sh) with BINARY: the target
# runs OpenSSH (installed BEFORE pairing so the setup user is registered), the client runs one real
# `pb ssh <target> -- echo <marker>` while every Paperboat address is blackholed in BOTH containers
# (the API, the tunnel edges, the relays, IPv4 and IPv6: the target's own resolution of every server-owned
# hostname in live discovery, unioned with the client's, so no DNS view leaves the host answering). The host
# records origin offline only when the client sent the offline header (pb ssh always does:
# localdaemon/source.go hostRulesPeerStream) AND its signed network config is past its refresh hint
# (tailnet/authority.go OutOfSync: now >= current.ExpiresAt, server/internal/peersessions/network.go:459
# admission.Expiry = cli_auth mint_proof_lifetime, 2 min by default). The outage is therefore 150 s
# (> 120 s hint); the 45 s outage of ssh_control_outage.sh is too short for 'offline'. A verify step right
# after the routes land (before the outage window) requires every blocked address to time out from the
# target; a completed connect fails the check with counts only, never the addresses. After the routes are
# removed the host uploads within its 30 s cadence; the check reads paperboat.audit_events (read-only, the
# operator access cleanup_test_account.py uses) and requires: an offline-origin start and its end (same
# session id, outcome closed, bytes_in/out > 0).
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

# Every Paperboat address, blackholed in target AND client (no server is touched): the API host from the
# target's own config plus every server-owned hostname in live discovery (installer, console, verification,
# machines, control regions, tunnel/preview/runtime base domains; the local-access alias domain is peer DNS,
# never blocked, so the direct peer path stays open). Each hostname resolves to A+AAAA on the TARGET, unioned
# with the client's view, so a DNS view that differs between the two cannot leave the host answering (its
# signed network stays current until EVERY refresh fails: tailnet/authority.go OutOfSync, past the 2 min hint).
api=$(ssh "$HOST" "sudo -n docker exec -u pbt $TARGET sed -n 's/.*\"server_url\": *\"https:\\/\\/\\([^\"]*\\)\".*/\\1/p' /home/pbt/.config/paperboat/config.json" 2>/dev/null)
[ -n "$api" ] || finish FAIL "could not read server_url from the target container"
disc=$(ssh "$HOST" "sudo -n docker exec $TARGET curl -fsS -m 10 https://$api/v1/client-configuration" 2>/dev/null)
[ -n "$disc" ] || finish FAIL "could not read discovery from the target container"
hosts=$(python3 - "$api" "$disc" <<'PY'
import json, sys
from urllib.parse import urlparse
api, disc = sys.argv[1], sys.argv[2]
try:
    data = json.loads(disc).get('data', {})
except ValueError:
    data = {}
if not isinstance(data, dict):
    data = {}
found = {api.lower().rstrip('.')}
def add_url(value):
    if not isinstance(value, str):
        return
    try:
        host = urlparse(value).hostname
    except ValueError:
        return
    if host:
        found.add(host.lower().rstrip('.'))
for key in ('issuer', 'installer_base_url', 'console_url', 'cli_verification_url', 'machines_url'):
    add_url(data.get(key))
for region in data.get('control_regions') or []:
    if isinstance(region, dict):
        add_url(region.get('api_origin'))
for key in ('preview_base_domain', 'tunnel_base_domain', 'runtime_base_domain'):
    value = data.get(key)
    if isinstance(value, str) and value and '/' not in value and ' ' not in value:
        found.add(value.lower().rstrip('.'))
# local_access_base_domain is peer alias DNS, not a server: blocking it could break the direct peer path.
print(' '.join(sorted(found)))
PY
)
[ -n "$hosts" ] || finish FAIL "no Paperboat hostname in discovery"
taddrs=$(ssh "$HOST" "sudo -n docker exec $TARGET bash -c 'for h in $hosts; do getent ahosts \$h 2>/dev/null | awk -v H=\$h \"{print H\\\"|\\\"\\\$1}\"; done' " 2>/dev/null)
caddrs=$(ssh "$HOST" "sudo -n docker exec $CLIENT bash -c 'for h in $hosts; do getent ahosts \$h 2>/dev/null | awk \"{print \$1}\"; done' " 2>/dev/null)
resolved=$(python3 - "$taddrs" "$caddrs" <<'PY'
import ipaddress, sys
pairs, seen = [], set()
for line in sys.argv[1].splitlines():
    host, sep, token = line.strip().rpartition('|')
    if not sep or not host:
        continue
    try:
        ip = str(ipaddress.ip_address(token))
    except ValueError:
        continue
    if (host, ip) not in seen:
        seen.add((host, ip))
        pairs.append(host + '|' + ip)
addrs = set(ip for _, ip in (p.split('|') for p in pairs))
for token in sys.argv[2].split():
    try:
        addrs.add(str(ipaddress.ip_address(token)))
    except ValueError:
        pass
print(' '.join(pairs))
print(' '.join(sorted(addrs, key=lambda a: (ipaddress.ip_address(a).version, a))))
PY
)
tpairs=$(sed -n '1p' <<<"$resolved")
ADDRS=$(sed -n '2p' <<<"$resolved")
[ -n "$ADDRS" ] || finish FAIL "could not resolve any Paperboat address"
naddrs=$(echo "$ADDRS" | wc -w | tr -d ' ')
for ct in $TARGET $CLIENT; do
  for a in $ADDRS; do
    case "$a" in
      *:*) ssh "$HOST" "sudo -n docker exec $ct ip -6 route add blackhole $a" 2>/dev/null ;;
      *) ssh "$HOST" "sudo -n docker exec $ct ip route add blackhole $a" 2>/dev/null ;;
    esac || finish FAIL "could not block all Paperboat addresses ($naddrs)"
  done
done
# The block must already hold before the outage window: from the target, every (hostname, address) pair the
# target itself resolves must fail to connect. The SNI pins each check to its hostname: these frontends abort
# nameless TLS, so an IP-only probe cannot tell blocked from open, while a TCP connect can (a blackhole fails
# it at once here; a timeout is the same verdict). Only a completed connect fails the check, with counts only,
# never the addresses. The block list stays the wider target+client union; the verify proves the mechanism on
# every address the target could still hear Paperboat on.
# each pair is double-quoted for the remote for-list: the | separator is a shell operator unquoted
qpairs=
for p in $tpairs; do qpairs="$qpairs \"$p\""; done
verify=$(ssh "$HOST" "sudo -n docker exec -i $TARGET bash -s" <<EOF 2>/dev/null
bad=0; total=0
for p in $qpairs; do
  h=\${p%%|*}; a=\${p##*|}; total=\$((total+1))
  if curl -s -o /dev/null -m 6 --connect-timeout 5 -k --resolve "\$h:443:\$a" "https://\$h/" 2>/dev/null; then bad=\$((bad+1)); fi
done
echo "VERIFY bad=\$bad total=\$total"
EOF
)
bad=; total=; read -r bad total <<<"$(sed -n 's/.*VERIFY bad=\([0-9]*\) total=\([0-9]*\).*/\1 \2/p' <<<"$verify")"
[ -n "$bad" ] && [ -n "$total" ] || finish FAIL "could not verify the control-plane block from the target"
[ "$total" -gt 0 ] || finish FAIL "control-plane verify ran no checks"
[ "$bad" = 0 ] || finish FAIL "control-plane verify failed: $bad of $total addresses still connect"

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
python3 - "$rows" "$outage_wall" <<'PY' && finish PASS "offline start and end audited after ${outage_wall}s outage ($naddrs addresses blocked): $(tr '\n' ' ' <<<"$rows" | grep -o 'offline|[^ ]*' | head -2 | tr '\n' ' '; tr '\n' ' ' <<<"$rows" | cut -c1-250)" || finish FAIL "offline audit events wrong or missing after ${outage_wall}s outage ($naddrs addresses blocked): $(tr '\n' ' ' <<<"$rows" | cut -c1-400)"
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
