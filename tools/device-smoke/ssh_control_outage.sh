#!/bin/bash
# ssh_control_outage.sh HOST BINARY  (check id: ssh-control-outage, CHECKLIST F5-08)
# SSH keeps working while Paperboat is unreachable (F5-08). Pairs three FRESH throwaway containers on HOST (as
# ssh_other_user.sh): the target, an unrevoked client C and a client R. R signs out (`pb logout` revokes its access)
# and the check waits, with the control plane still up, until R is refused by the host, i.e. the host has the
# withdrawal. Then the control plane's addresses are blackholed inside all three containers only (no server is
# touched) and new plain ssh and `pb ssh` sessions from C must still log in with the managed key while R is still
# refused. The routes are removed again before the containers go.
# Prints one JSON line {"check":"ssh-control-outage",...}; machines revoked by exact id, containers removed on exit.
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/ct_env.sh"
set -uo pipefail
. "$(cd "$(dirname "$0")" && pwd)/stage_binary.sh"
HOST=${1:?host} BIN=${2:?binary}
HERE=$(cd "$(dirname "$0")" && pwd)
STAMP=$(date +%s)
T=test-sco-t-$STAMP C=test-sco-c-$STAMP R=test-sco-r-$STAMP ALIAS=test-sco-$STAMP
IDS=
finish() {
  local status=$1 detail=$2 id
  ssh "$HOST" "$CT_DOCKER rm -f $T $C $R >/dev/null 2>&1; rm -f /tmp/$T-pb" || true
  for id in $IDS; do python3 "$HERE/cleanup_test_account.py" --id "$id" --stale '0 seconds' --apply >/dev/null 2>&1 || detail="$detail; revoke of $id failed"; done
  python3 -c 'import json,sys;print(json.dumps({"check":"ssh-control-outage","status":sys.argv[1],"machine_ids":sys.argv[2].split(),"detail":sys.argv[3]}))' "$status" "$IDS" "$detail"
  [ "$status" = PASS ]
  exit $?
}
stage_binary "$BIN" "$HOST" "/tmp/$T-pb" || finish FAIL "could not copy the build to $HOST"
out=$(ssh "$HOST" "bash -s" <<EOF 2>&1
set -euo pipefail
fresh() { # fresh CONTAINER NAME PACKAGES
  local ct=\$1 name=\$2 packages=\$3
  local X="$CT_DOCKER exec -u $CT_USER -e HOME=$CT_HOME -e XDG_RUNTIME_DIR=$CT_RUNDIR \$ct"
  $CT_DOCKER run -d --name \$ct --privileged --dns-option single-request --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock $CT_IMAGE >/dev/null
  sleep 8
  $CT_DOCKER exec \$ct bash -c "DEBIAN_FRONTEND=noninteractive apt-get -qq update >/dev/null 2>&1; DEBIAN_FRONTEND=noninteractive apt-get -qq install -y \$packages >/dev/null 2>&1; true"
  $CT_DOCKER exec \$ct loginctl enable-linger $CT_USER
  $CT_DOCKER exec \$ct systemctl start user@$CT_UID.service
  $CT_DOCKER cp /tmp/$T-pb \$ct:$CT_HOME/pb
  $CT_DOCKER exec \$ct bash -c 'chown $CT_USER:$CT_USER $CT_HOME/pb; chmod 755 $CT_HOME/pb'
  \$X bash -c 'cd ~ && ./pb install >/dev/null'
  NAME=\$name TOKEN=/tmp/\$ct-token python3 - <<'PY'
import json, os, re, subprocess
added = json.loads(subprocess.run([os.path.expanduser("~/.local/bin/pb"), "machine", "add", "--name", os.environ["NAME"], "--json"], capture_output=True, text=True, check=True).stdout)
token = re.search(r"PB_TOKEN='([A-Za-z0-9]+)'", added["data"]["linux_macos_command"]).group(1)
with open(os.open(os.environ["TOKEN"], os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600), "w") as f:
    f.write(token)
PY
  $CT_DOCKER cp /tmp/\$ct-token \$ct:$CT_HOME/token; rm -f /tmp/\$ct-token
  $CT_DOCKER exec \$ct bash -c 'chown $CT_USER:$CT_USER $CT_HOME/token; chmod 600 $CT_HOME/token'
  \$X $CT_HOME/.local/bin/pb pair --enrollment-token-file $CT_HOME/token --name \$name --json | python3 -c 'import json,sys;d=json.load(sys.stdin);assert d["ok"],d;print("MACHINE_ID="+d["data"]["machine_id"])'
  \$X rm -f $CT_HOME/token
}
# the target: OpenSSH (paired AFTER sshd runs, so the setup user pbt is registered with port 22) 
fresh $T $ALIAS "openssh-server"
$CT_DOCKER exec $T bash -c "systemctl enable --now ssh >/dev/null 2>&1"
fresh $C $ALIAS-c "openssh-client"
fresh $R $ALIAS-r "openssh-client"
EOF
)
IDS=$(sed -n 's/^MACHINE_ID=\(machine_[0-9a-f-]*\)$/\1/p' <<<"$out" | tr '\n' ' ')
[ -n "$IDS" ] || finish FAIL "pairing failed: $(tail -c 300 <<<"$out" | tr '\n' ' ')"
cat > /tmp/$T-run.sh <<'RUN'
set -u
ALIAS=$1 PHASE=$2
PB=~/.local/bin/pb
DOMAIN=
for i in $(seq 1 60); do DOMAIN=$(grep -o 'Host \*\.[^ ]*' ~/.ssh/paperboat_config 2>/dev/null | head -1 | sed 's/Host \*\.//'); [ -n "$DOMAIN" ] && break; sleep 2; done
HOST=$ALIAS.$DOMAIN
if [ "$PHASE" = warm ]; then
  for i in $(seq 1 60); do "$PB" machine list 2>/dev/null | grep -q "$ALIAS " && break; sleep 2; done
  for i in $(seq 1 30); do ssh "$HOST" true </dev/null >/dev/null 2>&1 && break; sleep 2; done
  ssh "$HOST" echo warm-ok </dev/null 2>&1
  exit 0
fi
if [ "$PHASE" = revoke ]; then # warm session first (proves R was allowed), then sign out
  for i in $(seq 1 60); do "$PB" machine list 2>/dev/null | grep -q "$ALIAS " && break; sleep 2; done
  for i in $(seq 1 30); do ssh "$HOST" true </dev/null >/dev/null 2>&1 && break; sleep 2; done
  ssh "$HOST" echo before-revoke-ok </dev/null 2>&1 | grep -q before-revoke-ok || { echo "RESULT revoke=no-baseline"; exit 0; }
  "$PB" logout >/dev/null 2>&1
  echo "RESULT revoke=signed-out"; exit 0
fi
plain=$(timeout 40 ssh -o BatchMode=yes -o ConnectTimeout=20 "$HOST" echo outage-plain-ok </dev/null 2>&1 | tr '\n' ' ')
pbout=$(timeout 40 "$PB" ssh "$ALIAS" -- echo outage-pb-ok </dev/null 2>&1 | tr '\n' ' ')
api=$(curl -s -m 5 -o /dev/null -w '%{http_code}' "https://$(sed -n 's/.*"server_url": *"https:\/\/\([^"/]*\).*/\1/p' ~/.config/paperboat/config.json)/v1/client-configuration" || true)
echo "RESULT api=$api plain=[$plain] pb=[$pbout]"
RUN
for ct in $C $R; do ssh "$HOST" "$CT_DOCKER exec -i -u $CT_USER $ct sh -c 'cat > $CT_HOME/run.sh'" < /tmp/$T-run.sh; done; rm -f /tmp/$T-run.sh
X="$CT_DOCKER exec -u $CT_USER -e HOME=$CT_HOME -e XDG_RUNTIME_DIR=$CT_RUNDIR $C timeout 300 bash $CT_HOME/run.sh $ALIAS"
warm=$(ssh "$HOST" "$X warm" 2>&1)
grep -q warm-ok <<<"$warm" || finish FAIL "no session before the outage: $(tail -c 200 <<<"$warm" | tr '\n' ' ')"
# The host's managed keys: each client publishes its own key. The exact keys are read BEFORE R signs out (a sign-out
# may delete the client's key file, which would make "absent" vacuous); R's must leave the target's authorized_keys
# (the host has the withdrawal) and stay out before, during and after the outage; C's must stay in. A live login with
# R's key is not possible: the private key never leaves the daemon's credential store. sshd must really consult
# authorized_keys (sshd -T), or its content proves nothing.
RX="$CT_DOCKER exec -u $CT_USER -e HOME=$CT_HOME -e XDG_RUNTIME_DIR=$CT_RUNDIR $R timeout 300 bash $CT_HOME/run.sh $ALIAS"
pubkey() { ssh "$HOST" "$CT_DOCKER exec -u $CT_USER $1 awk '{print \$2}' $CT_HOME/.ssh/paperboat-managed-ssh.pub"; }
RK=$(pubkey $R); CK=$(pubkey $C)
[ -n "$RK" ] && [ -n "$CK" ] && [ "$RK" != "$CK" ] || finish FAIL "could not read two distinct managed public keys (R=${#RK} C=${#CK} chars)"
listed() { ssh "$HOST" "$CT_DOCKER exec $T grep -qF \"$1\" $CT_HOME/.ssh/authorized_keys"; }
sshd=$(ssh "$HOST" "$CT_DOCKER exec $T sshd -T -C user=$CT_USER,host=localhost,addr=127.0.0.1" 2>&1)
grep -qiE '^authorizedkeysfile .*\.ssh/authorized_keys' <<<"$sshd" && grep -qiE '^authorizedkeyscommand none' <<<"$sshd" || finish FAIL "sshd does not read only ~/.ssh/authorized_keys: $(grep -iE '^authorizedkeys' <<<"$sshd" | tr '\n' ' ')"
for i in $(seq 1 30); do listed "$RK" && listed "$CK" && break; sleep 2; done
listed "$CK" || finish FAIL "the unrevoked client's managed key is not in the target's authorized_keys"
listed "$RK" || finish FAIL "the revoked client's key was never in the target's authorized_keys (nothing to withdraw)"
revoke=$(ssh "$HOST" "$RX revoke" 2>&1)
grep -q 'revoke=signed-out' <<<"$revoke" || finish FAIL "R could not sign out: $(tail -c 200 <<<"$revoke" | tr '\n' ' ')"
start=$(date +%s); withdrawn=
for i in $(seq 1 45); do listed "$RK" || { withdrawn=$(( $(date +%s)-start )); break; }; sleep 2; done
[ -n "$withdrawn" ] || finish FAIL "R signed out but the target still lists its key after 90 s"
# the control plane's addresses, blackholed in all containers (iproute2 in the image), removed on every exit
res=$(ssh "$HOST" "bash -s" <<EOF 2>&1
api=\$($CT_DOCKER exec -u $CT_USER $C sed -n 's/.*"server_url": *"https:\\/\\/\\([^"/]*\\).*/\\1/p' $CT_HOME/.config/paperboat/config.json)
addrs=\$($CT_DOCKER exec $C getent ahosts "\$api" | awk '{print \$1}' | sort -u)
for ct in $T $C $R; do for a in \$addrs; do $CT_DOCKER exec \$ct ip route add blackhole "\$a" 2>/dev/null || $CT_DOCKER exec \$ct ip -6 route add blackhole "\$a" 2>/dev/null; done; done
sleep 45
for who in R C; do k=\$([ \$who = R ] && echo "$RK" || echo "$CK"); $CT_DOCKER exec $T grep -qF "\$k" $CT_HOME/.ssh/authorized_keys && echo "KEYS-DURING \$who=listed" || echo "KEYS-DURING \$who=absent"; done
$X outage
echo "R-\$($RX outage | grep ^RESULT)"
for ct in $T $C $R; do for a in \$addrs; do $CT_DOCKER exec \$ct ip route del blackhole "\$a" 2>/dev/null || $CT_DOCKER exec \$ct ip -6 route del blackhole "\$a" 2>/dev/null; done; done
EOF
)
detail=$(grep -E "^(RESULT|R-RESULT|KEYS-DURING)" <<<"$res" | tr '\n' ' ')
# after the outage the target still lists C's exact key and not R's
listed "$CK" && ! listed "$RK" && keysok=yes || keysok=no
detail="withdrawn_after=${withdrawn}s keys_after_outage_ok=$keysok $detail"
if [ "$keysok" = yes ] && grep -q 'KEYS-DURING R=absent' <<<"$detail" && grep -q 'KEYS-DURING C=listed' <<<"$detail" && grep -q ' RESULT api=000 plain=\[[^]]*outage-plain-ok' <<<"$detail" && grep -q ' pb=\[[^]]*outage-pb-ok' <<<"$detail" && ! grep -q 'R-RESULT.*outage-\(plain\|pb\)-ok' <<<"$detail"; then
  finish PASS "$detail"
fi
finish FAIL "$detail $(tail -c 300 <<<"$res" | tr '\n' ' ')"
