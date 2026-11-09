#!/bin/bash
# ssh_live_revoke.sh HOST BINARY  (check id: ssh-live-revoke, CHECKLIST F5-10)
# Revoking access ends open SSH sessions (F5-10). Pairs two FRESH throwaway containers on HOST (as
# ssh_other_user.sh), the target with OpenSSH; the client opens `pb ssh <target>` running a long command, then
# `pb logout` revokes its CLI session. BOTH open sessions (`pb ssh` and a plain `ssh <alias>.<domain>` through the managed
# OpenSSH config) must end within 15 s and pb must say access was revoked ("access to <m> was revoked").
# Prints one JSON line {"check":"ssh-live-revoke",...}; machines revoked by exact id, containers removed on exit.
set -uo pipefail
HOST=${1:?host} BIN=${2:?binary}
HERE=$(cd "$(dirname "$0")" && pwd)
STAMP=$(date +%s)
T=lanei-slr-t-$STAMP C=lanei-slr-c-$STAMP ALIAS=lanei-slr-$STAMP
IDS=
finish() {
  local status=$1 detail=$2 id
  ssh "$HOST" "sudo -n docker rm -f $T $C >/dev/null 2>&1; rm -f /tmp/$T-pb" || true
  for id in $IDS; do python3 "$HERE/cleanup_test_account.py" --id "$id" --stale '0 seconds' --apply >/dev/null 2>&1 || detail="$detail; revoke of $id failed"; done
  python3 -c 'import json,sys;print(json.dumps({"check":"ssh-live-revoke","status":sys.argv[1],"machine_ids":sys.argv[2].split(),"detail":sys.argv[3]}))' "$status" "$IDS" "$detail"
  [ "$status" = PASS ]
  exit $?
}
scp -q "$BIN" "$HOST:/tmp/$T-pb" || finish FAIL "could not copy the build to $HOST"
out=$(ssh "$HOST" "bash -s" <<EOF 2>&1
set -euo pipefail
fresh() { # fresh CONTAINER NAME PACKAGES
  local ct=\$1 name=\$2 packages=\$3
  local X="sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 \$ct"
  sudo -n docker run -d --name \$ct --privileged --dns-option single-request --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock laneh-ubuntu24-systemd >/dev/null
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
ALIAS=$1
PB=~/.local/bin/pb
for i in $(seq 1 60); do "$PB" machine list 2>/dev/null | grep -q "$ALIAS " && break; sleep 2; done
DOMAIN=
for i in $(seq 1 60); do DOMAIN=$(grep -o 'Host \*\.[^ ]*' ~/.ssh/paperboat_config 2>/dev/null | head -1 | sed 's/Host \*\.//'); [ -n "$DOMAIN" ] && break; sleep 2; done
# Two open sessions to the target: `pb ssh` and a plain ssh through the managed OpenSSH config (pb __ssh-proxy).
"$PB" ssh "$ALIAS" -- 'echo session-up; sleep 300' > ~/slr.out 2> ~/slr.err & pid=$!
ssh "$ALIAS.$DOMAIN" 'echo plain-up; sleep 300' > ~/slrp.out 2> ~/slrp.err < /dev/null & ppid=$!
for i in $(seq 1 60); do grep -q session-up ~/slr.out && grep -q plain-up ~/slrp.out && break; sleep 1; done
grep -q session-up ~/slr.out && grep -q plain-up ~/slrp.out || { echo "RESULT fail:[no-session pb=$(tr '\n' ' ' < ~/slr.err | cut -c1-120) plain=$(tr '\n' ' ' < ~/slrp.err | cut -c1-120)]"; exit 0; }
start=$(date +%s.%N); "$PB" logout >/dev/null 2>&1
for i in $(seq 1 600); do kill -0 $pid 2>/dev/null || break; sleep 0.1; done
end=$(date +%s.%N)
for i in $(seq 1 600); do kill -0 $ppid 2>/dev/null || break; sleep 0.1; done
pend=$(date +%s.%N)
palive=$(kill -0 $ppid 2>/dev/null && echo yes || echo no); alive=$(kill -0 $pid 2>/dev/null && echo yes || echo no); kill $pid $ppid 2>/dev/null
echo "RESULT seconds=$(python3 -c "print(round($end-$start,1))") alive=$alive plain_seconds=$(python3 -c "print(round($pend-$start,1))") plain_alive=$palive stderr=[$(tr '\n' ' ' < ~/slr.err | cut -c1-200)] plain_stderr=[$(tr '\n' ' ' < ~/slrp.err | cut -c1-200)]"
RUN
ssh "$HOST" "sudo -n docker exec -i -u pbt $C sh -c 'cat > /home/pbt/run.sh'" < /tmp/$T-run.sh; rm -f /tmp/$T-run.sh
res=$(ssh "$HOST" "sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 $C timeout 600 bash /home/pbt/run.sh $ALIAS" 2>&1)
detail=$(grep -E "^(domain=|RESULT)" <<<"$res" | tr '\n' ' ')
secs=$(sed -n 's/^RESULT seconds=\([0-9.]*\) .*/\1/p' <<<"$res")
psecs=$(sed -n 's/^RESULT .* plain_seconds=\([0-9.]*\) .*/\1/p' <<<"$res")
if [ -n "$secs" ] && [ -n "$psecs" ] && python3 -c "import sys; sys.exit(0 if float('$secs') <= 15 and float('$psecs') <= 15 else 1)" && grep -q "alive=no" <<<"$res" && grep -q "plain_alive=no" <<<"$res" && grep -qiE "access (to [^ ]+ )?was revoked" <<<"$res"; then
  finish PASS "$detail"
fi
finish FAIL "$detail $(tail -c 300 <<<"$res" | tr '\n' ' ')"
