#!/bin/bash
# revoke_client.sh up HOST NAME BINARY | down HOST NAME [MACHINE_ID]
# The fresh throwaway client R of ssh-control-outage (F5-08) and ssh-live-revoke (F5-10): a disposable systemd container NAME
# (image test-systemd, passwordless sudo and ip for pbt) on HOST with BINARY installed and paired as a machine of the SAME
# account the smoke runs as, i.e. the account HOST's own ~/.local/bin/pb is signed in to (as ssh_live_revoke.sh's cli mode).
#   up    prints one JSON line {"name","machine_id","command"}. `command` is REVOKE_CLIENT_CMD: a shell prefix that
#         takes ONE shell-string argument and runs it as pb's user in the container (ssh_mac_rows.py run()). On any failure
#         it removes what it made, says why on stderr (token-like strings redacted) and exits 1.
#   down  removes the container and revokes the machine by exact id (cleanup_test_account.py --id ... --apply); no
#         MACHINE_ID: only the container goes. Exit 1 when the revoke failed. Safe to run twice.
# The enrollment token goes from HOST's `pb machine add` straight into the container's stdin; it is never printed or
# written to HOST's disk.
# Wiring: ssh_mac_rows.py (so windows_checks.py ssh-control-outage / ssh-live-revoke and the Mac checks) calls this when
# REVOKE_CLIENT_CMD is unset and REVOKE_CLIENT_HOST (+ REVOKE_CLIENT_BIN, a Linux build for HOST) is set: up before the
# step, down after it, even on failure.
# env: REVOKE_CLIENT_CLEANUP overrides the revoke command (a test hook; default python3 cleanup_test_account.py).
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
. "$HERE/stage_binary.sh"
usage() { echo "usage: $0 up HOST NAME BINARY | down HOST NAME [MACHINE_ID]" >&2; exit 2; }
ACTION=${1:-} HOST=${2:-} NAME=${3:-}
[ -n "$HOST" ] && [[ $NAME =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || usage
CLEANUP=${REVOKE_CLIENT_CLEANUP:-"python3 $HERE/cleanup_test_account.py"}
redact() { sed -E "s/[A-Za-z0-9_+\/=-]{24,}/<redacted>/g"; }

down() { # down MACHINE_ID: the container, the staged build, then the machine record by exact id
  local rc=0
  ssh "$HOST" "sudo -n docker rm -f $NAME >/dev/null 2>&1; rm -f /tmp/$NAME-pb" || true
  if [ -n "${1:-}" ]; then
    $CLEANUP --id "$1" --stale '0 seconds' --apply >/dev/null 2>&1 || { echo "revoke_client: revoke of $1 failed" >&2; rc=1; }
  fi
  return $rc
}

up() {
  local bin=${1:?binary} out id fail
  fail() { echo "revoke_client: $1" >&2; down ""; exit 1; }
  stage_binary "$bin" "$HOST" "/tmp/$NAME-pb" || fail "could not copy the build to $HOST"
  out=$(ssh "$HOST" "ct=$NAME bash -s" 2>&1 <<'EOF'
set -euo pipefail
X="sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 $ct"
sudo -n docker rm -f $ct >/dev/null 2>&1 || true
sudo -n docker run -d --name $ct --privileged --dns-option single-request --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock test-systemd >/dev/null
sleep 8
sudo -n docker exec $ct loginctl enable-linger pbt
sudo -n docker exec $ct systemctl start user@1000.service
sudo -n docker cp /tmp/$ct-pb $ct:/home/pbt/pb
sudo -n docker exec $ct bash -c 'chown pbt:pbt /home/pbt/pb; chmod 755 /home/pbt/pb'
$X bash -c 'cd ~ && ./pb install >/dev/null'
python3 - "$ct" <<'PY' | sudo -n docker exec -i -u pbt $ct sh -c 'umask 077; cat > /home/pbt/token'
import json, os, re, subprocess, sys
added = json.loads(subprocess.run([os.path.expanduser("~/.local/bin/pb"), "machine", "add", "--name", sys.argv[1], "--json"], capture_output=True, text=True, check=True).stdout)
sys.stdout.write(re.search(r"PB_TOKEN='([A-Za-z0-9]+)'", added["data"]["linux_macos_command"]).group(1))
PY
$X /home/pbt/.local/bin/pb pair --enrollment-token-file /home/pbt/token --name $ct --json | python3 -c 'import json,sys;d=json.load(sys.stdin);assert d["ok"],d;print("MACHINE_ID="+d["data"]["machine_id"])'
$X rm -f /home/pbt/token
EOF
  ) || true
  id=$(sed -n 's/^MACHINE_ID=\(machine_[0-9a-f-]*\)$/\1/p' <<<"$out" | head -1)
  [ -n "$id" ] || fail "pairing the throwaway client on $HOST failed: $(tail -c 300 <<<"$out" | tr '\n' ' ' | redact)"
  python3 -c 'import json,shlex,sys
host, name, mid = sys.argv[1:]
print(json.dumps({"name": name, "machine_id": mid, "command": "ssh %s sudo -n docker exec -i -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 %s" % (shlex.quote(host), name)}))' "$HOST" "$NAME" "$id"
}

case $ACTION in
  up) up "${4:-}" ;;
  down) down "${4:-}" ;;
  *) usage ;;
esac
