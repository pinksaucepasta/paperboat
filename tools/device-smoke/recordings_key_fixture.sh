#!/bin/bash
# recordings_key_fixture.sh up HOST NAME TEAM | down HOST NAME TEAM [MACHINE_ID] | ensure-key HOST TEAM
# The team-visible TARGET of the recording-key-rotation check (PB-R1): a disposable systemd container NAME (image SMOKE_CT_IMAGE, default test-systemd)
# on HOST, running a copy of HOST's own ~/.local/bin/pb (the team owner's session), paired as a machine of that owner and
# then transferred to TEAM (`pb team machine transfer-to-team`; a machine that is only shared is invisible to
# `pb session list --workspace TEAM`). Modelled on revoke_client.sh.
#   up    prints one JSON line {"name","machine_id"}; on any failure removes what it made, says why on stderr (token-like
#         strings redacted) and exits 1.
#   down  removes the machine from the team, revokes it by exact id (`pb machine revoke ID --json`, then --confirm TOKEN),
#         removes the container and the staged build. No MACHINE_ID: only the container and files go. Exit 1 when the
#         revoke failed. Safe to run twice.
#   ensure-key  exits 0 when TEAM has a recording key (generation >= 1), creating generation 1 through the CLI when it can;
#         otherwise says why on stderr and exits 1 (never a skip).
# env (the harness's own, smoke.sh/final_recheck.py): SMOKE_CT_DOCKER (default "sudo -n docker"), SMOKE_CT_USER (pbt),
# SMOKE_CT_UID (1000), and SMOKE_CT_IMAGE (test-systemd; gap5-systemd on hosts that have no test-systemd).
# The enrollment token goes from HOST's `pb machine add` straight into the container's stdin; it is never printed.
set -uo pipefail
usage() { echo "usage: $0 up HOST NAME TEAM | down HOST NAME TEAM [MACHINE_ID] | ensure-key HOST TEAM" >&2; exit 2; }
ACTION=${1:-} HOST=${2:-} NAME=${3:-} TEAM=${4:-}
[ "$ACTION" = ensure-key ] && { TEAM=${3:-}; NAME=ensure-key; }
[ -n "$HOST" ] && [ -n "$TEAM" ] && [[ $NAME =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] && [[ $TEAM =~ ^[A-Za-z0-9_-]+$ ]] || usage
IMAGE=${SMOKE_CT_IMAGE:-test-systemd} CT_USER=${SMOKE_CT_USER:-pbt} CT_UID=${SMOKE_CT_UID:-1000} DOCKER=${SMOKE_CT_DOCKER:-sudo -n docker}
[[ $IMAGE =~ ^[A-Za-z0-9][A-Za-z0-9._/:-]*$ && $CT_USER =~ ^[a-z_][a-z0-9_-]*$ && $CT_UID =~ ^[0-9]+$ && $DOCKER =~ ^[a-z\ -]+$ ]] || usage
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
  ssh "$HOST" "$DOCKER rm -f $NAME >/dev/null 2>&1; rm -rf /tmp/$NAME-pb /tmp/$NAME-files" || true
  return $rc
}

up() {
  local out id fail
  fail() { echo "recordings_key_fixture: $1" >&2; down "${2:-}"; exit 1; }
  out=$(ssh "$HOST" "ct=$NAME team=$TEAM image=$IMAGE cu=$CT_USER uid=$CT_UID docker='$DOCKER' bash -s" 2>&1 <<'EOF2'
set -euo pipefail
PB=$HOME/.local/bin/pb
X="$docker exec -u $cu -e HOME=/home/$cu -e XDG_RUNTIME_DIR=/run/user/$uid $ct"
cp "$PB" /tmp/$ct-pb
$docker rm -f $ct >/dev/null 2>&1 || true
$docker run -d --name $ct --privileged --dns-option single-request --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock $image >/dev/null
sleep 8
$docker exec $ct loginctl enable-linger $cu
$docker exec $ct systemctl start user@$uid.service
$docker cp /tmp/$ct-pb $ct:/home/$cu/pb
$docker exec $ct bash -c 'chown $cu:$cu /home/$cu/pb; chmod 755 /home/$cu/pb'
$X bash -c 'cd ~ && ./pb install >/dev/null'
python3 - "$ct" "$PB" <<'PY' | $docker exec -i -u $cu $ct sh -c 'umask 077; cat > /home/$cu/token'
import json, re, subprocess, sys
added = json.loads(subprocess.run([sys.argv[2], "machine", "add", "--name", sys.argv[1], "--json"], capture_output=True, text=True, check=True).stdout)
sys.stdout.write(re.search(r"PB_TOKEN='([A-Za-z0-9]+)'", added["data"]["linux_macos_command"]).group(1))
PY
mid=$($X /home/$cu/.local/bin/pb pair --enrollment-token-file /home/$cu/token --name $ct --json | python3 -c 'import json,sys;d=json.load(sys.stdin);assert d["ok"],d;print(d["data"]["machine_id"])')
$X rm -f /home/$cu/token
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

# The one place to change when the CLI can read or create a team's first recording key: it looks for the subcommands the
# CLI really has (pb recordings key --help) and uses them; none yet means a loud failure, not a skip.
ensure_key() {
  local help gen c pb='$HOME/.local/bin/pb'
  help=$(ssh "$HOST" "$pb recordings key --help" 2>&1) || { echo "recordings_key_fixture: pb recordings key --help failed on $HOST" >&2; return 1; }
  if grep -qE '^ +status( |$)' <<<"$help"; then
    gen=$(ssh "$HOST" "$pb recordings key status --team $TEAM --json" 2>/dev/null | python3 -c 'import json,sys;d=json.load(sys.stdin);d=d.get("data",d);print(d.get("generation",0))' 2>/dev/null)
    [ "${gen:-0}" -ge 1 ] 2>/dev/null && return 0
  fi
  for c in init create setup enable; do
    if grep -qE "^ +$c( |\$)" <<<"$help"; then
      ssh "$HOST" "$pb recordings key $c --team $TEAM --confirm $TEAM --json" >/dev/null 2>&1 && return 0
      echo "recordings_key_fixture: pb recordings key $c failed for $TEAM" >&2; return 1
    fi
  done
  echo "team has no recording key and the CLI cannot create generation 1 yet" >&2
  return 1
}

case $ACTION in
  ensure-key) ensure_key ;;
  up) up ;;
  down) down "${5:-}" ;;
  *) usage ;;
esac
