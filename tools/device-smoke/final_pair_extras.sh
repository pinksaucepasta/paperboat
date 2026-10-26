#!/bin/bash
# final_pair_extras.sh MODE HOST BINARY: final re-check checks that need fresh throwaway pairs, on fresh throwaway containers on HOST
# (a Docker host with the laneh-ubuntu24-systemd image; HOST's own pb mints the enrollment tokens, as in fresh_pair_lib.sh).
#
#   exec     two fresh pairs with BINARY, then `pb exec` from one to the other as the very first thing after the pair
#            (no wait, no retry): the old intermittent first-exec failure. Passes only if the marker comes back.
#   revoke   C036: two fresh pairs; one revokes the other by exact id (`pb machine revoke`, revoked_machine.py revoke),
#            then the revoked machine checks it lost access at once and does not hammer the server (verify).
#   history  two machines installed with the PUBLISHED one-liner (the official Latest), real history on the target
#            (background preview, tunnel, a held terminal, a transfer in flight), then `pb update --from BINARY` while
#            all of it runs: transfer byte-identical, preview and tunnel on the same URLs, the same shell answers
#            (update_survival.py --terminal).
#   scheduled one machine installed with the PUBLISHED one-liner, then update_scheduled.py: `pb update settings` disables and
#            enables the automatic update, and at the configured time the machine reaches the newest official release by
#            itself (about 15 minutes). The official Latest must be older than the newest release: cut the candidate as a
#            release first; otherwise this FAILS with that reason.
#
# Prints one JSON line {"check":"final-extras-MODE","status":"PASS"|"FAIL",...}. Machines are revoked by exact id and the
# containers removed on every exit path; enrollment tokens stay in files.
set -uo pipefail
MODE=${1:?exec|revoke|history|scheduled} HOST=${2:?host} BIN=${3:?binary}
HERE=$(cd "$(dirname "$0")" && pwd)
STAMP=$(date +%s)
# A Docker host whose user needs no sudo for docker (hp): SMOKE_CT_DOCKER=docker turns the scripts' `sudo -n docker` into `docker`.
SHIM=; [ "${SMOKE_CT_DOCKER:-}" = docker ] && SHIM='sudo() { shift; "$@"; }'
T=final-$MODE-t-$STAMP P=final-$MODE-p-$STAMP ALIAS=lanea-fx-$STAMP
MACHINE_IDS=

finish() {
  local status=$1 detail=$2 id
  ssh "$HOST" "${SMOKE_CT_DOCKER:-sudo -n docker} rm -f $T $P >/dev/null 2>&1; rm -f /tmp/$T-pb /tmp/$T-ds.tgz" || true
  for id in $MACHINE_IDS; do
    python3 "$HERE/cleanup_test_account.py" --id "$id" --stale '0 seconds' --apply >/dev/null 2>&1 || detail="$detail; revoke of $id failed"
  done
  python3 -c 'import json,sys;print(json.dumps({"check":"final-extras-"+sys.argv[1],"status":sys.argv[2],"machine_ids":sys.argv[3].split(),"detail":sys.argv[4]}))' "$MODE" "$status" "$MACHINE_IDS" "$detail"
  [ "$status" = PASS ]
  exit $?
}

scp -q "$BIN" "$HOST:/tmp/$T-pb" || finish FAIL "could not copy the build to $HOST"
tar czf /tmp/$T-ds.tgz -C "$HERE" $(cd "$HERE" && ls *.py *.sh) && scp -q /tmp/$T-ds.tgz "$HOST:/tmp/$T-ds.tgz"; rm -f /tmp/$T-ds.tgz

if [ "$MODE" = exec ]; then
  out=$( { echo "FRESH_BINARY=/tmp/$T-pb T=$T P=$P ALIAS=$ALIAS STAMP=$STAMP"; echo "$SHIM"; cat "$HERE/fresh_pair_lib.sh"; cat <<'EOF'; } | ssh "$HOST" "bash -s" 2>&1
set -euo pipefail
fresh $T $ALIAS
fresh $P $ALIAS-p
X="sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000"
$X $P bash -c "echo FRESH_EXEC_$STAMP > ~/marker"
if $X $T timeout 60 /home/pbt/.local/bin/pb exec $ALIAS-p -- cat /home/pbt/marker | grep -qx FRESH_EXEC_$STAMP; then echo EXEC_OK; else echo EXEC_FAILED; fi
EOF
)
  MACHINE_IDS=$(sed -n 's/^MACHINE_ID=\(machine_[0-9a-f-]*\)$/\1/p' <<<"$out" | tr '\n' ' ')
  grep -qx EXEC_OK <<<"$out" && finish PASS "pb exec worked as the first command after a fresh pair, no retry"
  finish FAIL "$(grep -v "^MACHINE_ID=" <<<"$out" | tail -n 3 | cut -c1-300 | tr "\n" " ")"
fi

if [ "$MODE" = scheduled ]; then
  out=$( { echo "FRESH_BINARY=/tmp/$T-pb T=$T ALIAS=$ALIAS"; echo "$SHIM"; cat "$HERE/fresh_pair_lib.sh"; cat <<'EOF'; } | ssh "$HOST" "bash -s" 2>&1
set -euo pipefail
X="sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000"
start_ct $T
sudo -n docker exec $T bash -c "loginctl enable-linger pbt; systemctl start user@1000.service"
${FRESH_MINT:-$HOME/.local/bin/pb} machine add --name $ALIAS --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["data"]["linux_macos_command"])' > /tmp/$T-cmd
sudo -n docker cp /tmp/$T-cmd $T:/home/pbt/install-cmd; rm -f /tmp/$T-cmd
$X $T bash -c 'bash /home/pbt/install-cmd > /home/pbt/install.log 2>&1; rm -f /home/pbt/install-cmd'
$X $T python3 -c 'import json;print("MACHINE_ID="+json.load(open("/home/pbt/.local/state/paperboat/runtime/machine-registration.json"))["machine_id"])'
sudo -n docker cp /tmp/$T-ds.tgz $T:/home/pbt/ds.tgz
$X $T bash -c 'mkdir -p ~/ds && tar xzf ~/ds.tgz -C ~/ds'
if res=$($X $T bash -c "cd ~/ds && python3 update_scheduled.py" 2>&1); then echo SCHEDULED_OK; else echo SCHEDULED_FAILED; fi
echo "$res" | tail -n 3
EOF
)
  MACHINE_IDS=$(sed -n 's/^MACHINE_ID=\(machine_[0-9a-f-]*\)$/\1/p' <<<"$out" | tr '\n' ' ')
  grep -qx SCHEDULED_OK <<<"$out" && finish PASS "official install: settings disable/enable persisted; at the configured time it reached the newest release by itself"
  finish FAIL "$(grep -v "^MACHINE_ID=" <<<"$out" | tail -n 3 | cut -c1-300 | tr "\n" " ")"
fi

if [ "$MODE" = revoke ]; then
  out=$( { echo "FRESH_BINARY=/tmp/$T-pb T=$T P=$P ALIAS=$ALIAS STAMP=$STAMP"; cat "$HERE/fresh_pair_lib.sh"; cat <<'EOF'; } | ssh "$HOST" "bash -s" 2>&1
set -euo pipefail
fresh $T $ALIAS
fresh $P $ALIAS-p
X="sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000"
for ct in $T $P; do sudo -n docker cp /tmp/$T-ds.tgz $ct:/home/pbt/ds.tgz; $X $ct bash -c 'mkdir -p ~/ds && tar xzf ~/ds.tgz -C ~/ds'; done
target=$($X $T python3 -c 'import json;print(json.load(open("/home/pbt/.local/state/paperboat/runtime/machine-registration.json"))["machine_id"])')
for i in $(seq 1 24); do $X $P timeout 40 /home/pbt/.local/bin/pb exec $ALIAS -- true >/dev/null 2>&1 && break; sleep 5; done
if $X $P bash -c "cd ~/ds && python3 revoked_machine.py revoke $target" && $X $T bash -c "cd ~/ds && python3 revoked_machine.py verify $ALIAS-p --window 60"; then echo REVOKE_OK; else echo REVOKE_FAILED; fi
EOF
)
  MACHINE_IDS=$(sed -n 's/^MACHINE_ID=\(machine_[0-9a-f-]*\)$/\1/p' <<<"$out" | tr '\n' ' ')
  grep -qx REVOKE_OK <<<"$out" && finish PASS "pb machine revoke by exact id: the revoked machine lost access at once and stayed quiet"
  finish FAIL "$(grep -v "^MACHINE_ID=" <<<"$out" | tail -n 3 | cut -c1-300 | tr "\n" " ")"
fi

# history: the official Latest comes from the server's own published enrollment command, run inside each container.
out=$( { echo "FRESH_BINARY=/tmp/$T-pb T=$T P=$P ALIAS=$ALIAS"; echo "$SHIM"; cat "$HERE/fresh_pair_lib.sh"; cat <<'EOF'; } | ssh "$HOST" "bash -s" 2>&1
set -euo pipefail
X="sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000"
for ct in $T $P; do
  start_ct $ct
  sudo -n docker exec $ct bash -c "loginctl enable-linger pbt; systemctl start user@1000.service"
  name=$ALIAS; [ $ct = $P ] && name=$ALIAS-p
  ${FRESH_MINT:-$HOME/.local/bin/pb} machine add --name $name --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["data"]["linux_macos_command"])' > /tmp/$ct-cmd
  sudo -n docker cp /tmp/$ct-cmd $ct:/home/pbt/install-cmd; rm -f /tmp/$ct-cmd
  $X $ct bash -c 'bash /home/pbt/install-cmd > /home/pbt/install.log 2>&1; rm -f /home/pbt/install-cmd'
  $X $ct python3 -c 'import json;print("MACHINE_ID="+json.load(open("/home/pbt/.local/state/paperboat/runtime/machine-registration.json"))["machine_id"])'
  echo "OFFICIAL_$ct=$($X $ct /home/pbt/.local/bin/pb --version | sed -n 2p | tr -s ' ')"
done
sudo -n docker cp /tmp/$T-pb $T:/home/pbt/final-pb
sudo -n docker cp /tmp/$T-ds.tgz $T:/home/pbt/ds.tgz
$X $T bash -c 'chmod 755 ~/final-pb && mkdir -p ~/ds && tar xzf ~/ds.tgz -C ~/ds'
# the update is what is judged here (first exec after a pair is the exec mode's job): wait until the pair is usable
for i in $(seq 1 24); do $X $T timeout 40 /home/pbt/.local/bin/pb exec $ALIAS-p -- true >/dev/null 2>&1 && break; sleep 5; done
if echo x | $X $T bash -c "cd ~/ds && python3 update_survival.py $ALIAS $ALIAS-p --terminal --mib 400 --from /home/pbt/final-pb"; then echo HISTORY_OK; else echo HISTORY_FAILED; fi
echo "FINAL_$T=$($X $T /home/pbt/.local/bin/pb --version | sed -n 2p | tr -s ' ')"
EOF
)
MACHINE_IDS=$(sed -n 's/^MACHINE_ID=\(machine_[0-9a-f-]*\)$/\1/p' <<<"$out" | tr '\n' ' ')
official=$(sed -n 's/^OFFICIAL_[^=]*=//p' <<<"$out" | head -1) final=$(sed -n 's/^FINAL_[^=]*=//p' <<<"$out" | head -1)
grep -qx HISTORY_OK <<<"$out" && finish PASS "official '$official' updated in place to '$final' with a preview, a tunnel, a held terminal and a transfer running; all survived"
finish FAIL "$(grep -v "^MACHINE_ID=\|^OFFICIAL_\|^FINAL_" <<<"$out" | tail -n 3 | cut -c1-300 | tr "\n" " ")"
