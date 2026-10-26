#!/bin/bash
# ssh_control_outage.sh HOST BINARY  (check id: ssh-control-outage, CHECKLIST F5-08)
# SSH keeps working while Paperboat is unreachable (F5-08). Pairs two FRESH throwaway containers on HOST (as
# ssh_other_user.sh), connects once, then blackholes the control plane's addresses inside both containers only (no
# server is touched) and requires new plain ssh and `pb ssh` sessions to still log in with the managed key. The
# routes are removed again before the containers go.
# Prints one JSON line {"check":"ssh-control-outage",...}; machines revoked by exact id, containers removed on exit.
set -uo pipefail
HOST=${1:?host} BIN=${2:?binary}
HERE=$(cd "$(dirname "$0")" && pwd)
STAMP=$(date +%s)
T=lanei-sco-t-$STAMP C=lanei-sco-c-$STAMP ALIAS=lanei-sco-$STAMP
IDS=
finish() {
  local status=$1 detail=$2 id
  ssh "$HOST" "sudo -n docker rm -f $T $C >/dev/null 2>&1; rm -f /tmp/$T-pb" || true
  for id in $IDS; do python3 "$HERE/cleanup_test_account.py" --id "$id" --stale '0 seconds' --apply >/dev/null 2>&1 || detail="$detail; revoke of $id failed"; done
  python3 -c 'import json,sys;print(json.dumps({"check":"ssh-control-outage","status":sys.argv[1],"machine_ids":sys.argv[2].split(),"detail":sys.argv[3]}))' "$status" "$IDS" "$detail"
  [ "$status" = PASS ]
  exit $?
}
scp -q "$BIN" "$HOST:/tmp/$T-pb" || finish FAIL "could not copy the build to $HOST"
out=$(ssh "$HOST" "bash -s" <<EOF 2>&1
set -euo pipefail
fresh() { # fresh CONTAINER NAME PACKAGES
  local ct=\$1 name=\$2 packages=\$3
  local X="sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 \$ct"
  sudo -n docker run -d --name \$ct --privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock laneh-ubuntu24-systemd >/dev/null
  sleep 8
  sudo -n docker exec \$ct bash -c "DEBIAN_FRONTEND=noninteractive apt-get -qq update >/dev/null 2>&1; DEBIAN_FRONTEND=noninteractive apt-get -qq install -y \$packages >/dev/null 2>&1; true"
  sudo -n docker exec \$ct loginctl enable-linger pbt
  sudo -n docker exec \$ct systemctl start user@1000.service
  sudo -n docker cp /tmp/$T-pb \$ct:/home/pbt/pb
  sudo -n docker exec \$ct bash -c 'chown pbt:pbt /home/pbt/pb; chmod 755 /home/pbt/pb'
  \$X bash -c 'cd ~ && ./pb install >/dev/null'
  NAME=\$name TOKEN=/tmp/\$ct-token python3 - <<'PY'
import json, os, re, subprocess
added = json.loads(subprocess.run([os.path.expanduser("~/.local/bin/pb"), "machine", "add", "--name", os.environ["NAME"], "--json"], capture_output=True, text=True, check=True).stdout)
url = re.search(r"'(https?://[^']+)'", added["data"]["linux_macos_command"]).group(1)
script = subprocess.run(["curl", "-fsSL", url], capture_output=True, text=True, check=True).stdout
token = re.search(r"PAPERBOAT_ENROLLMENT_TOKEN='([A-Za-z0-9]+)'", script).group(1)
with open(os.open(os.environ["TOKEN"], os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600), "w") as f:
    f.write(token)
PY
  sudo -n docker cp /tmp/\$ct-token \$ct:/home/pbt/token; rm -f /tmp/\$ct-token
  sudo -n docker exec \$ct bash -c 'chown pbt:pbt /home/pbt/token; chmod 600 /home/pbt/token'
  \$X /home/pbt/.local/bin/pb pair --enrollment-token-file /home/pbt/token --name \$name --json | python3 -c 'import json,sys;d=json.load(sys.stdin);assert d["ok"],d;print("MACHINE_ID="+d["data"]["machine_id"])'
  \$X rm -f /home/pbt/token
}
# the target: OpenSSH (paired AFTER sshd runs, so the setup user pbt is registered with port 22) 
fresh $T $ALIAS "openssh-server"
sudo -n docker exec $T bash -c "systemctl enable --now ssh >/dev/null 2>&1"
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
plain=$(ssh "$HOST" echo outage-plain-ok </dev/null 2>&1 | tr '\n' ' ')
pbout=$("$PB" ssh "$ALIAS" -- echo outage-pb-ok </dev/null 2>&1 | tr '\n' ' ')
api=$(curl -s -m 5 -o /dev/null -w '%{http_code}' "https://$(sed -n 's/.*"server_url": *"https:\/\/\([^"/]*\).*/\1/p' ~/.config/paperboat/config.json)/v1/client-configuration" || true)
echo "RESULT api=$api plain=[$plain] pb=[$pbout]"
RUN
ssh "$HOST" "sudo -n docker exec -i -u pbt $C sh -c 'cat > /home/pbt/run.sh'" < /tmp/$T-run.sh; rm -f /tmp/$T-run.sh
X="sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 $C timeout 300 bash /home/pbt/run.sh $ALIAS"
warm=$(ssh "$HOST" "$X warm" 2>&1)
grep -q warm-ok <<<"$warm" || finish FAIL "no session before the outage: $(tail -c 200 <<<"$warm" | tr '\n' ' ')"
# the control plane's addresses, blackholed in both containers (iproute2 in the image), removed on every exit
res=$(ssh "$HOST" "bash -s" <<EOF 2>&1
api=\$(sudo -n docker exec -u pbt $C sed -n 's/.*"server_url": *"https:\\/\\/\\([^"/]*\\).*/\\1/p' /home/pbt/.config/paperboat/config.json)
addrs=\$(sudo -n docker exec $C getent ahosts "\$api" | awk '{print \$1}' | sort -u)
for ct in $T $C; do for a in \$addrs; do sudo -n docker exec \$ct ip route add blackhole "\$a" 2>/dev/null || sudo -n docker exec \$ct ip -6 route add blackhole "\$a" 2>/dev/null; done; done
sleep 45
$X outage
for ct in $T $C; do for a in \$addrs; do sudo -n docker exec \$ct ip route del blackhole "\$a" 2>/dev/null || sudo -n docker exec \$ct ip -6 route del blackhole "\$a" 2>/dev/null; done; done
EOF
)
detail=$(grep -E "^RESULT" <<<"$res" | tr '\n' ' ')
if grep -q 'api=000' <<<"$detail" && grep -q 'plain=\[[^]]*outage-plain-ok' <<<"$detail" && grep -q 'pb=\[[^]]*outage-pb-ok' <<<"$detail"; then
  finish PASS "$detail"
fi
finish FAIL "$detail $(tail -c 300 <<<"$res" | tr '\n' ' ')"
