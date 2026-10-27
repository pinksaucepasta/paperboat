#!/bin/bash
# F5-04: a machine's SSH host key changes; pb ssh refuses it and names the pending identity, pb ssh trust-host
# trusts exactly that fingerprint, and pb ssh works again. Runs on the runner: ssh_trust_host.sh HOST CT_A CT_B PEER
# (CT_A dials PEER, which runs in CT_B; the key rotation needs root in CT_B).
set -euo pipefail
host=$1 a=$2 b=$3 peer=$4
pbt="sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000"
fail() { echo "{\"check\": \"ssh-trust-host\", \"status\": \"FAIL\", \"reason\": \"$1\"}"; exit 1; }

ssh "$host" "$pbt $a /home/pbt/.local/bin/pb ssh $peer -- true" || fail "pb ssh failed before the key change"
ssh "$host" "sudo -n docker exec $b sh -c 'rm -f /etc/ssh/ssh_host_ed25519_key /etc/ssh/ssh_host_ed25519_key.pub && ssh-keygen -q -t ed25519 -N \"\" -f /etc/ssh/ssh_host_ed25519_key && (systemctl reload ssh || systemctl reload sshd) && systemctl restart paperboat-hostd-u1000'"

# The restarted host runtime observes the new key; the server holds it as pending until the owner trusts it.
pending=""
for _ in $(seq 1 30); do
  out=$(ssh "$host" "$pbt $a /home/pbt/.local/bin/pb ssh $peer -- true 2>&1" || true)
  pending=$(grep -o 'pb ssh trust-host [^ ]* --fingerprint [^ `]*' <<<"$out" | awk '{print $NF}' | head -1)
  [ -n "$pending" ] && break
  sleep 5
done
[ -n "$pending" ] || fail "pb ssh did not name a pending host identity after the key change"

ssh "$host" "$pbt $a /home/pbt/.local/bin/pb ssh trust-host $peer --fingerprint wrong-fingerprint --json" >/dev/null 2>&1 &&
  fail "trust-host accepted a fingerprint that is not the pending one"
ssh "$host" "$pbt $a /home/pbt/.local/bin/pb ssh trust-host $peer --fingerprint $pending --json" | grep -q '"trusted": *true' ||
  fail "trust-host did not trust the pending fingerprint"
# The host installs authorized keys for the newly trusted set on its next reconcile (about a minute on hp).
ok=""
for _ in $(seq 1 18); do
  ssh "$host" "$pbt $a /home/pbt/.local/bin/pb ssh $peer -- true" >/dev/null 2>&1 && { ok=1; break; }
  sleep 5
done
[ -n "$ok" ] || fail "pb ssh failed for 90 s after trusting the new host key"
echo "{\"check\": \"ssh-trust-host\", \"target\": \"$peer\", \"status\": \"PASS\", \"pending_named\": true, \"wrong_fingerprint_refused\": true, \"trusted_then_connected\": true}"
