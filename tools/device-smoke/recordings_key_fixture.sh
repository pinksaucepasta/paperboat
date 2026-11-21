#!/bin/bash
# recordings_key_fixture.sh up HOST NAME TEAM | down HOST NAME TEAM [MACHINE_ID]
# The team-visible TARGET of the recording-key-rotation check (PB-R1): a disposable systemd container NAME (image test-systemd)
# on HOST, running a copy of HOST's own ~/.local/bin/pb (the team owner's session), paired as a machine of that owner and
# then transferred to TEAM (`pb team machine transfer-to-team`; a machine that is only shared is invisible to
# `pb session list --workspace TEAM`). Modelled on revoke_client.sh.
#   up    prints one JSON line {"name","machine_id"}; on any failure removes what it made, says why on stderr (token-like
#         strings redacted) and exits 1.
#   down  removes the machine from the team, revokes it by exact id (`pb machine revoke ID --json`, then --confirm TOKEN),
#         removes the container and the staged build. No MACHINE_ID: only the container and files go. Exit 1 when the
#         revoke failed. Safe to run twice.
# The enrollment token goes from HOST's `pb machine add` straight into the container's stdin; it is never printed.
set -uo pipefail
usage() { echo "usage: $0 up HOST NAME TEAM | down HOST NAME TEAM [MACHINE_ID]" >&2; exit 2; }
ACTION=${1:-} HOST=${2:-} NAME=${3:-} TEAM=${4:-}
[ -n "$HOST" ] && [ -n "$TEAM" ] && [[ $NAME =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] && [[ $TEAM =~ ^[A-Za-z0-9_-]+$ ]] || usage
redact() { sed -E "s/[A-Za-z0-9_+\/=-]{24,}/<redacted>/g"; }
# On HOST: run pb as the owner and print data fields of its --json answer.
PBH='$HOME/.local/bin/pb'

down() { # down MACHINE_ID
  local rc=0 id=${1:-}
  if [ -n "$id" ]; then
    ssh "$HOST" "id=$id team=$TEAM bash -s" >/dev/null 2>&1 <<'EOF2' || { echo "recordings_key_fixture: revoke of $id failed" >&2; rc=1; }
PB=$HOME/.local/bin/pb
gen=$($PB team get "$team" --json | python3 -c 'import json,sys;d=json.load(sys.stdin);d=d.get("data",d);print(d["generation"])') && \
  $PB team machine remove "$team" "$id" --confirm "$id" --generation "$gen" --json >/dev/null 2>&1
for ws in "" "--workspace personal"; do
  token=$($PB $ws machine revoke "$id" --json 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)["data"]["confirmation_token"])') || continue
  $PB $ws machine revoke "$id" --json --confirm "$token" >/dev/null 2>&1 && exit 0
done
exit 1
EOF2
  fi
  ssh "$HOST" "sudo -n docker rm -f $NAME >/dev/null 2>&1; rm -rf /tmp/$NAME-pb /tmp/$NAME-files" || true
  return $rc
}

up() {
  local out id fail
  fail() { echo "recordings_key_fixture: $1" >&2; down "${2:-}"; exit 1; }
  out=$(ssh "$HOST" "ct=$NAME team=$TEAM bash -s" 2>&1 <<'EOF2'
set -euo pipefail
PB=$HOME/.local/bin/pb
X="sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 $ct"
cp "$PB" /tmp/$ct-pb
sudo -n docker rm -f $ct >/dev/null 2>&1 || true
sudo -n docker run -d --name $ct --privileged --dns-option single-request --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock test-systemd >/dev/null
sleep 8
sudo -n docker exec $ct loginctl enable-linger pbt
sudo -n docker exec $ct systemctl start user@1000.service
sudo -n docker cp /tmp/$ct-pb $ct:/home/pbt/pb
sudo -n docker exec $ct bash -c 'chown pbt:pbt /home/pbt/pb; chmod 755 /home/pbt/pb'
$X bash -c 'cd ~ && ./pb install >/dev/null'
python3 - "$ct" "$PB" <<'PY' | sudo -n docker exec -i -u pbt $ct sh -c 'umask 077; cat > /home/pbt/token'
import json, re, subprocess, sys
added = json.loads(subprocess.run([sys.argv[2], "machine", "add", "--name", sys.argv[1], "--json"], capture_output=True, text=True, check=True).stdout)
sys.stdout.write(re.search(r"PB_TOKEN='([A-Za-z0-9]+)'", added["data"]["linux_macos_command"]).group(1))
PY
mid=$($X /home/pbt/.local/bin/pb pair --enrollment-token-file /home/pbt/token --name $ct --json | python3 -c 'import json,sys;d=json.load(sys.stdin);assert d["ok"],d;print(d["data"]["machine_id"])')
$X rm -f /home/pbt/token
echo "MACHINE_ID=$mid"
gen=$($PB team get "$team" --json | python3 -c 'import json,sys;d=json.load(sys.stdin);d=d.get("data",d);print(d["generation"])')
$PB team machine transfer-to-team "$team" "$mid" --confirm "$mid" --generation "$gen" --json >/dev/null
$PB team get "$team" --json | python3 -c 'import json,sys;d=json.load(sys.stdin);d=d.get("data",d);m=[x for x in d.get("machines",[]) if x["machine_id"]==sys.argv[1] and x["active"]];assert m, "the machine is not an active team machine"' "$mid"
echo TEAM_VISIBLE=1
EOF2
  ) || true
  id=$(sed -n 's/^MACHINE_ID=\(machine_[0-9a-f-]*\)$/\1/p' <<<"$out" | head -1)
  [ -n "$id" ] || fail "pairing the throwaway machine on $HOST failed: $(tail -c 300 <<<"$out" | tr '\n' ' ' | redact)"
  grep -qx TEAM_VISIBLE=1 <<<"$out" || fail "the machine did not become a team machine of $TEAM: $(tail -c 300 <<<"$out" | tr '\n' ' ' | redact)" "$id"
  python3 -c 'import json,sys;print(json.dumps({"name": sys.argv[1], "machine_id": sys.argv[2]}))' "$NAME" "$id"
}

case $ACTION in
  up) up ;;
  down) down "${5:-}" ;;
  *) usage ;;
esac
