#!/bin/bash
# fresh_pair_access.sh HOST BINARY
#
# Pairs a FRESH throwaway container on HOST (a Linux VM with Docker and the laneh-ubuntu24-systemd image, whose
# user `pb` is signed in to the test account) with BINARY, then runs `pb access machine <it> --port N` from HOST
# to a marker origin on the container, end to end, with no daemon restart in between. Machines paired before a
# regression never show a missing local-address lease (the private-access P0 of 2026-10-26); only a fresh pair
# does. Prints one JSON line {"check":"fresh-pair-access","status":"PASS"|"FAIL",...}; the machine is revoked by
# exact id and the container removed on every exit path. The enrollment token stays in files.
set -uo pipefail
HOST=${1:?host} BIN=${2:?binary}
HERE=$(cd "$(dirname "$0")" && pwd)
STAMP=$(date +%s)
CT=fresh-access-$STAMP ALIAS=lanea-fpa-$STAMP PORT=18473 LISTEN=127.0.0.1:18474
MACHINE_ID=

finish() {
  local status=$1 detail=$2
  ssh "$HOST" "sudo -n docker rm -f $CT >/dev/null 2>&1; rm -f /tmp/$CT-pb" || true
  if [ -n "$MACHINE_ID" ]; then
    python3 "$HERE/cleanup_test_account.py" --id "$MACHINE_ID" --stale '0 seconds' --apply >/dev/null 2>&1 || detail="$detail; revoke of $MACHINE_ID failed"
  fi
  python3 -c 'import json,sys;print(json.dumps({"check":"fresh-pair-access","status":sys.argv[1],"machine_id":sys.argv[2],"detail":sys.argv[3]}))' "$status" "$MACHINE_ID" "$detail"
  [ "$status" = PASS ]
  exit $?
}

scp -q "$BIN" "$HOST:/tmp/$CT-pb" || finish FAIL "could not copy the build to $HOST"
out=$(ssh "$HOST" "bash -s" <<EOF 2>&1
set -euo pipefail
X="sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 $CT"
sudo -n docker run -d --name $CT --privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock laneh-ubuntu24-systemd >/dev/null
sleep 8
sudo -n docker exec $CT loginctl enable-linger pbt
sudo -n docker exec $CT systemctl start user@1000.service
sudo -n docker cp /tmp/$CT-pb $CT:/home/pbt/pb
sudo -n docker exec $CT bash -c 'chown pbt:pbt /home/pbt/pb; chmod 755 /home/pbt/pb'
\$X bash -c 'cd ~ && ./pb install >/dev/null'
python3 - <<'PY'
import json, os, re, subprocess
added = json.loads(subprocess.run([os.path.expanduser("~/.local/bin/pb"), "machine", "add", "--name", "$ALIAS", "--json"], capture_output=True, text=True, check=True).stdout)
command = added["data"]["linux_macos_command"]
url = re.search(r"'(https?://[^']+)'", command).group(1)
script = subprocess.run(["curl", "-fsSL", url], capture_output=True, text=True, check=True).stdout
token = re.search(r"PAPERBOAT_ENROLLMENT_TOKEN='([A-Za-z0-9]+)'", script).group(1)
path = "/tmp/$CT-token"
with open(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600), "w") as f:
    f.write(token)
PY
sudo -n docker cp /tmp/$CT-token $CT:/home/pbt/token
rm -f /tmp/$CT-token
sudo -n docker exec $CT bash -c 'chown pbt:pbt /home/pbt/token; chmod 600 /home/pbt/token'
\$X /home/pbt/.local/bin/pb pair --enrollment-token-file /home/pbt/token --name $ALIAS --json | python3 -c 'import json,sys;d=json.load(sys.stdin);assert d["ok"],d;print("MACHINE_ID="+d["data"]["machine_id"])'
\$X bash -c 'rm -f ~/token; mkdir -p ~/origin && echo FRESH_ACCESS_$STAMP > ~/origin/marker && cd ~/origin && nohup python3 -m http.server $PORT --bind 127.0.0.1 >/dev/null 2>&1 &'
timeout 150 ~/.local/bin/pb access machine $ALIAS --port $PORT --listen $LISTEN > /tmp/$CT-access.log 2>&1 &
ACCESS=\$!
for i in \$(seq 1 120); do
  if curl -fsS --max-time 5 http://$LISTEN/marker 2>/dev/null | grep -qx FRESH_ACCESS_$STAMP; then echo ACCESS_OK; break; fi
  kill -0 \$ACCESS 2>/dev/null || { echo ACCESS_EXITED; tail -c 600 /tmp/$CT-access.log; break; }
  sleep 1
done
kill \$ACCESS 2>/dev/null || true
rm -f /tmp/$CT-access.log
EOF
)
MACHINE_ID=$(sed -n 's/^MACHINE_ID=\(machine_[0-9a-f-]*\)$/\1/p' <<<"$out" | head -1)
if grep -qx ACCESS_OK <<<"$out"; then
  finish PASS "fresh pair, then pb access machine reached the marker origin with no daemon restart"
fi
finish FAIL "$(tail -c 400 <<<"$out" | tr '\n' ' ')"
