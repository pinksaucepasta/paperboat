#!/bin/bash
# fixture_machine.sh up HOST NAME BINARY STATE | down HOST NAME [MACHINE_ID]
# A disposable systemd container NAME on HOST (image SMOKE_CT_IMAGE, built from its <image>.Dockerfile when HOST lacks it) with BINARY
# installed and paired as a machine of the console account in STATE (a Playwright storage state, mode 600, from console_session.mjs
# or fresh_account.mjs). The account is whoever STATE is: the Free fixture, the smoke team's owner, a member. HOST needs only Docker.
#   up    prints one JSON line {"name","machine_id","prefix"}; prefix is a shell prefix that runs ONE shell-string argument as the
#         container user in its home (`ssh HOST docker exec -i ... NAME`), and the pb path in it is $HOME/.local/bin/pb. On any
#         failure it removes what it made, says why on stderr (token-like strings redacted) and exits 1.
#   down  the machine revokes itself by exact id (its own session, `pb machine revoke ID`, then --confirm TOKEN), then the container
#         goes. No MACHINE_ID: only the container. Exit 1 when the revoke failed. Safe to run twice.
# env: ORIGIN, PW_CHROMIUM, [PW_MODULE] (as mint_enrollment_token.mjs); SMOKE_CT_* as in ct_env.sh. The enrollment token goes from
# the controller into the container's stdin; it is never printed.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
. "$HERE/stage_binary.sh"
. "$HERE/ct_env.sh"
usage() { echo "usage: $0 up HOST NAME BINARY STATE | down HOST NAME [MACHINE_ID]" >&2; exit 2; }
ACTION=${1:-} HOST=${2:-} NAME=${3:-}
[ -n "$HOST" ] && [[ $NAME =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || usage
redact() { sed -E "s/[A-Za-z0-9_+\/=-]{24,}/<redacted>/g"; }
PB=$CT_HOME/.local/bin/pb
XI="$CT_DOCKER exec -i -u $CT_USER -e HOME=$CT_HOME -e XDG_RUNTIME_DIR=$CT_RUNDIR -w $CT_HOME $NAME"
X="$CT_DOCKER exec -u $CT_USER -e HOME=$CT_HOME -e XDG_RUNTIME_DIR=$CT_RUNDIR -w $CT_HOME $NAME"

down() { # down MACHINE_ID
  local rc=0 id=${1:-}
  if [ -n "$id" ]; then
    ssh "$HOST" "$XI sh -s" >/dev/null 2>&1 <<EOF2 || { echo "fixture_machine: revoke of $id failed; revoke it by exact id" >&2; rc=1; }
t=\$($PB machine revoke $id --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["data"]["confirmation_token"])') && $PB machine revoke $id --json --confirm "\$t"
EOF2
  fi
  ssh -n "$HOST" "$CT_DOCKER rm -f $NAME >/dev/null 2>&1; rm -f /tmp/$NAME.pb" || true
  return $rc
}

ensure_image() { # build the harness image on HOST once, from $CT_IMAGE.Dockerfile
  ssh -n "$HOST" "$CT_DOCKER image inspect $CT_IMAGE >/dev/null 2>&1" && return 0
  [ -f "$HERE/$CT_IMAGE.Dockerfile" ] || { echo "image $CT_IMAGE is missing on $HOST and $CT_IMAGE.Dockerfile does not exist" >&2; return 1; }
  tar -C "$HERE" -cf - "$CT_IMAGE.Dockerfile" | ssh "$HOST" "d=\$(mktemp -d) && tar -xf - -C \$d && $CT_DOCKER build -q -t $CT_IMAGE -f \$d/$CT_IMAGE.Dockerfile \$d >/dev/null; rc=\$?; rm -rf \$d; exit \$rc"
}

up() {
  local bin=${4:-} state=${5:-} out id
  [ -r "$bin" ] && [ -r "$state" ] && [ -n "${ORIGIN:-}" ] && [ -n "${PW_CHROMIUM:-}" ] || usage
  fail() { echo "fixture_machine: $1" >&2; down "${2:-}"; exit 1; }
  ensure_image || fail "could not build the container image on $HOST"
  stage_binary "$bin" "$HOST" "/tmp/$NAME.pb" || fail "could not stage the build on $HOST"
  ssh -n "$HOST" "$CT_DOCKER rm -f $NAME >/dev/null 2>&1; $CT_DOCKER run -d --name $NAME --hostname $NAME --privileged --dns-option single-request --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock $CT_IMAGE >/dev/null && sleep 8 \
    && $CT_DOCKER exec $NAME loginctl enable-linger $CT_USER && $CT_DOCKER exec $NAME systemctl start user@$CT_UID.service \
    && $CT_DOCKER cp /tmp/$NAME.pb $NAME:$CT_HOME/pb && rm -f /tmp/$NAME.pb && $CT_DOCKER exec $NAME sh -c 'chown $CT_USER:$CT_USER $CT_HOME/pb && chmod 755 $CT_HOME/pb' \
    && $X ./pb install >/dev/null" 2>&1 | redact >&2
  [ "${PIPESTATUS[0]}" = 0 ] || fail "could not install the build in the container on $HOST"
  STATE=$state node "$HERE/mint_enrollment_token.mjs" | ssh "$HOST" "$CT_DOCKER exec -i -u $CT_USER $NAME sh -c 'umask 077; cat > $CT_HOME/token'" \
    || fail "could not mint or deliver the enrollment token (is the console session in $state still valid?)"
  out=$(ssh -n "$HOST" "$X $PB pair --enrollment-token-file $CT_HOME/token --name $NAME --json" 2>&1)
  ssh -n "$HOST" "$X rm -f $CT_HOME/token" >/dev/null 2>&1
  id=$(python3 -c 'import json,sys;d=json.loads(sys.argv[1]);d=d.get("data",d);print(d["machine_id"])' "$out" 2>/dev/null) \
    || fail "pairing failed: $(tail -c 300 <<<"$out" | tr '\n' ' ' | redact)"
  python3 -c 'import json,sys;print(json.dumps({"name":sys.argv[1],"machine_id":sys.argv[2],"prefix":sys.argv[3]}))' "$NAME" "$id" "ssh $HOST $X"
}

case $ACTION in
  up) up "$@" ;;
  down) down "${4:-}" ;;
  *) usage ;;
esac
