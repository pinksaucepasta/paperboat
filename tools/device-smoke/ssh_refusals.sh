#!/bin/bash
# ssh_refusals.sh HOST BINARY  (check id: ssh-refusals, CHECKLIST F5-09)
# A refused SSH connection says why in pb's own words (F5-09), not only OpenSSH's "Connection closed". Pairs
# two FRESH throwaway containers on HOST (as ssh_other_user.sh), connects once, then for each case plain ssh must
# print a "pb:" line naming it: the target's sshd stopped (sshd refusing / not set up), the target machine paused
# (offline), this computer's Paperboat service stopped. Each case is undone before the next. (No access is the
# workspace test TestOfflineSessionsStartFromTheHostsSignedScopes.)
# Prints one JSON line {"check":"ssh-refusals",...}; machines revoked by exact id, containers removed on exit.
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/ct_env.sh"
set -uo pipefail
. "$(cd "$(dirname "$0")" && pwd)/stage_binary.sh"
HOST=${1:?host} BIN=${2:?binary}
HERE=$(cd "$(dirname "$0")" && pwd)
STAMP=$(date +%s)
T=test-srf-t-$STAMP C=test-srf-c-$STAMP ALIAS=test-srf-$STAMP
IDS=
finish() {
  local status=$1 detail=$2 id
  ssh "$HOST" "$CT_DOCKER rm -f $T $C >/dev/null 2>&1; rm -f /tmp/$T-pb" || true
  for id in $IDS; do python3 "$HERE/cleanup_test_account.py" --id "$id" --stale '0 seconds' --apply >/dev/null 2>&1 || detail="$detail; revoke of $id failed"; done
  python3 -c 'import json,sys;print(json.dumps({"check":"ssh-refusals","status":sys.argv[1],"machine_ids":sys.argv[2].split(),"detail":sys.argv[3]}))' "$status" "$IDS" "$detail"
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
echo "CASE $PHASE [$(timeout 90 ssh "$HOST" true </dev/null 2>&1 | tr '\n' ' ' | cut -c1-300)]"
RUN
ssh "$HOST" "$CT_DOCKER exec -i -u $CT_USER $C sh -c 'cat > $CT_HOME/run.sh'" < /tmp/$T-run.sh; rm -f /tmp/$T-run.sh
X="$CT_DOCKER exec -u $CT_USER -e HOME=$CT_HOME -e XDG_RUNTIME_DIR=$CT_RUNDIR $C timeout 300 bash $CT_HOME/run.sh $ALIAS"
warm=$(ssh "$HOST" "$X warm" 2>&1)
grep -q warm-ok <<<"$warm" || finish FAIL "no session before the refusals: $(tail -c 200 <<<"$warm" | tr '\n' ' ')"
res=$(ssh "$HOST" "bash -s" <<EOF 2>&1
$CT_DOCKER exec $T bash -c 'systemctl stop ssh.socket ssh 2>/dev/null; true'
$X sshd_stopped
$CT_DOCKER exec $T bash -c 'systemctl start ssh 2>/dev/null; true'
sleep 5
$CT_DOCKER pause $T >/dev/null
$X machine_offline
$CT_DOCKER unpause $T >/dev/null
$CT_DOCKER exec -u $CT_USER -e XDG_RUNTIME_DIR=$CT_RUNDIR $C systemctl --user stop paperboatd
$X service_stopped
$CT_DOCKER exec -u $CT_USER -e XDG_RUNTIME_DIR=$CT_RUNDIR $C systemctl --user start paperboatd
EOF
)
detail=$(grep -E "^CASE " <<<"$res" | tr '\n' ' ')
ok=1
grep -qE "CASE sshd_stopped \[[^]]*pb: (the machine's SSH server closed|SSH is not set up)" <<<"$res" || ok=0
grep -qE "CASE machine_offline \[[^]]*pb: Paperboat could not reach " <<<"$res" || ok=0
grep -qE "CASE service_stopped \[[^]]*pb: this computer.s Paperboat service is not running" <<<"$res" || ok=0
[ "$ok" = 1 ] && finish PASS "$detail"
finish FAIL "$detail $(tail -c 300 <<<"$res" | tr '\n' ' ')"
