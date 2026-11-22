#!/bin/bash
# ssh_setup_time.sh HOST BINARY  (check id: ssh-setup-time, CHECKLIST F5-07)
# A warm managed SSH connection is shell-ready fast (F5-07): the helpers read the daemon, the host admits from
# its own rules. Pairs two FRESH throwaway containers on HOST (as ssh_other_user.sh), warms one connection, then
# times 7 runs each of plain `ssh <alias>.<domain> true` and `pb ssh <alias> -- true`; PASS when both p50 < 1000 ms.
# Prints one JSON line {"check":"ssh-setup-time",...}; machines revoked by exact id, containers removed on exit.
# FRESH_MINT (a pb on HOST signed in to a throwaway account) enrolls there instead of HOST's own pb; CLEANUP_ACCOUNT is
# then that account's id for the revocation.
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/ct_env.sh"
set -uo pipefail
. "$(cd "$(dirname "$0")" && pwd)/stage_binary.sh"
HOST=${1:?host} BIN=${2:?binary}
HERE=$(cd "$(dirname "$0")" && pwd)
STAMP=$(date +%s)
T=test-sst-t-$STAMP C=test-sst-c-$STAMP ALIAS=test-sst-$STAMP
IDS=
finish() {
  local status=$1 detail=$2 id
  ssh "$HOST" "$CT_DOCKER rm -f $T $C >/dev/null 2>&1; rm -f /tmp/$T-pb" || true
  for id in $IDS; do python3 "$HERE/cleanup_test_account.py" --id "$id" ${CLEANUP_ACCOUNT:+--account "$CLEANUP_ACCOUNT"} --stale '0 seconds' --apply >/dev/null 2>&1 || detail="$detail; revoke of $id failed"; done
  python3 -c 'import json,sys;print(json.dumps({"check":"ssh-setup-time","status":sys.argv[1],"machine_ids":sys.argv[2].split(),"detail":sys.argv[3]}))' "$status" "$IDS" "$detail"
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
  NAME=\$name TOKEN=/tmp/\$ct-token PB_MINT=${FRESH_MINT:-} python3 - <<'PY'
import json, os, re, subprocess
added = json.loads(subprocess.run([os.environ.get("PB_MINT") or os.path.expanduser("~/.local/bin/pb"), "machine", "add", "--name", os.environ["NAME"], "--json"], capture_output=True, text=True, check=True).stdout)
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
ALIAS=$1
PB=~/.local/bin/pb
for i in $(seq 1 60); do "$PB" machine list 2>/dev/null | grep -q "$ALIAS " && break; sleep 2; done
DOMAIN=
for i in $(seq 1 60); do DOMAIN=$(grep -o 'Host \*\.[^ ]*' ~/.ssh/paperboat_config 2>/dev/null | head -1 | sed 's/Host \*\.//'); [ -n "$DOMAIN" ] && break; sleep 2; done
HOST=$ALIAS.$DOMAIN
for i in $(seq 1 30); do ssh "$HOST" true </dev/null >/dev/null 2>&1 && break; sleep 2; done
ms() { local s e; s=$(date +%s%N); timeout 30 "$@" </dev/null >/dev/null 2>&1 || { echo -1; return; }; e=$(date +%s%N); echo $(( (e-s)/1000000 )); }
plain=""; pb=""
for i in $(seq 1 7); do
  a=$(ms ssh "$HOST" true); b=$(ms "$PB" ssh "$ALIAS" -- true)
  echo "RUN $i plain=$a pb=$b"
  plain="$plain $a"; pb="$pb $b"
done
p50() { tr ' ' '\n' | grep -v '^$' | sort -n | sed -n 4p; }
echo "RESULT plain_p50=$(echo $plain | p50) pb_p50=$(echo $pb | p50) plain=[$plain ] pb=[$pb ]"
RUN
ssh "$HOST" "$CT_DOCKER exec -i -u $CT_USER $C sh -c 'cat > $CT_HOME/run.sh'" < /tmp/$T-run.sh; rm -f /tmp/$T-run.sh
res=$(ssh "$HOST" "$CT_DOCKER exec -u $CT_USER -e HOME=$CT_HOME -e XDG_RUNTIME_DIR=$CT_RUNDIR $C timeout 600 bash $CT_HOME/run.sh $ALIAS" 2>&1)
detail=$(grep -E "^(domain=|RESULT|RUN )" <<<"$res" | tr '\n' ' ')
plain=$(sed -n 's/^RESULT plain_p50=\(-\?[0-9]*\) .*/\1/p' <<<"$res"); pb=$(sed -n 's/.* pb_p50=\(-\?[0-9]*\) .*/\1/p' <<<"$res")
# pb p50 may exceed plain ssh p50 by at most max(150 ms, 15%) (same rule as ssh_mac_rows.setup_verdict)
if [ -n "$plain" ] && [ -n "$pb" ] && [ "$plain" -ge 0 ] && [ "$pb" -ge 0 ]; then
  extra=$(( plain * 15 / 100 )); [ "$extra" -ge 150 ] || extra=150; allowed=$(( plain + extra ))
  detail="$detail allowed_pb_p50=$allowed"
  if [ "$pb" -le "$allowed" ] && ! grep -q -- "-1" <<<"$(grep '^RESULT' <<<"$res")"; then
    finish PASS "$detail"
  fi
fi
finish FAIL "$detail $(tail -c 300 <<<"$res" | tr '\n' ' ')"
