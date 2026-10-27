#!/bin/bash
# final_pair_extras.sh MODE HOST [PREVIOUS|CANDIDATE [WRONG_PLATFORM]]: final re-check checks that need fresh throwaway pairs, on fresh throwaway containers on HOST
# (a Docker host with the laneh-ubuntu24-systemd image). Every machine is installed with the published one-liner (the official
# Latest) and enrolled in the fresh throwaway account of the run: FRESH_MINT is a pb signed in to that account (final_recheck.py
# sets it up); there is no fallback to any other account.
#
#   exec     two fresh pairs, then `pb exec` from one to the other as the very first thing after the pair
#            (no wait, no retry): the old intermittent first-exec failure. Passes only if the marker comes back.
#   revoke   C036: two fresh pairs; one revokes the other by exact id (`pb machine revoke`, revoked_machine.py revoke),
#            then the revoked machine checks it lost access at once and does not hammer the server (verify).
#   history  the target is the PREVIOUS official release (asset of that release, installed and paired fresh; the Latest when
#            PREVIOUS is empty), the peer the official Latest; real history on the target (background preview, tunnel, a held
#            terminal, a transfer in flight), then `pb update download` + `--approve` of the Latest while all of it runs:
#            transfer byte-identical, preview and tunnel on the same URLs, the same shell answers (update_survival.py --terminal).
#            With a fourth argument, a local build file, the update goes to that build instead (`pb update --from FILE`, the dev-target form
#            used while no newer official release exists).
#   continuity  one machine on the official Latest, then update_continuity.py: an update to CANDIDATE (a different linux build)
#            refuses while a `pb ssh` connection is open, keeps a held terminal and a running exec, and retires the old session owner
#            (F14-02, F14-04).
#   failures one machine on the official Latest, then update_failures.py with CANDIDATE as the good build and WRONG_PLATFORM (a
#            binary of another platform): bad, truncated, wrong-platform and killed updates keep a working install (F14-03).
#   scheduled one machine installed with the published one-liner, then update_scheduled.py: `pb update settings` disables and
#            enables the automatic update, and at the configured time the machine reaches the newest official release by
#            itself (about 15 minutes). The official Latest must be older than the newest release: cut the candidate as a
#            release first; otherwise this FAILS with that reason. SCHEDULED_FROM_RELEASE=<older release> installs that
#            release instead of the newest (to test before a newer release exists).
#
# Prints one JSON line {"check":"final-extras-MODE","status":"PASS"|"FAIL",...}. The containers are removed on every exit path; their
# machines belong to the run's throwaway account, which the caller deletes. Enrollment tokens stay in files.
set -uo pipefail
MODE=${1:?exec|revoke|history|continuity|failures|scheduled} HOST=${2:?host} PREVIOUS=${3:-} WRONG=${4:-}
: "${FRESH_MINT:?the pb of the fresh throwaway account (see final_recheck.py)}"
HERE=$(cd "$(dirname "$0")" && pwd)
STAMP=$(date +%s)
# A Docker host whose user needs no sudo for docker (hp): SMOKE_CT_DOCKER=docker turns the scripts' `sudo -n docker` into `docker`.
SHIM=; [ "${SMOKE_CT_DOCKER:-}" = docker ] && SHIM='sudo() { shift; "$@"; }'
T=final-$MODE-t-$STAMP P=final-$MODE-p-$STAMP ALIAS=lanea-fx-$STAMP
MACHINE_IDS=

finish() {
  local status=$1 detail=$2
  ssh "$HOST" "${SMOKE_CT_DOCKER:-sudo -n docker} rm -f $T $P >/dev/null 2>&1; rm -f /tmp/$T-ds.tgz /tmp/$T-dev" || true
  python3 -c 'import json,sys;print(json.dumps({"check":"final-extras-"+sys.argv[1],"status":sys.argv[2],"machine_ids":sys.argv[3].split(),"detail":sys.argv[4]}))' "$MODE" "$status" "$MACHINE_IDS" "$detail"
  [ "$status" = PASS ]
  exit $?
}

tar czf /tmp/$T-ds.tgz -C "$HERE" $(cd "$HERE" && ls *.py *.sh) && scp -q /tmp/$T-ds.tgz "$HOST:/tmp/$T-ds.tgz"; [ "$HOST" = local ] || rm -f /tmp/$T-ds.tgz  # on the VM itself the copy is the file

if [ "$MODE" = exec ]; then
  out=$( { echo "FRESH_MINT=$FRESH_MINT T=$T P=$P ALIAS=$ALIAS STAMP=$STAMP"; echo "$SHIM"; cat "$HERE/fresh_pair_lib.sh"; cat <<'EOF'; } | ssh "$HOST" "bash -s" 2>&1
set -euo pipefail
fresh_official $T $ALIAS
fresh_official $P $ALIAS-p
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
  out=$( { echo "FRESH_MINT=$FRESH_MINT T=$T ALIAS=$ALIAS REL=${SCHEDULED_FROM_RELEASE:-}"; echo "$SHIM"; cat "$HERE/fresh_pair_lib.sh"; cat <<'EOF'; } | ssh "$HOST" "bash -s" 2>&1
set -euo pipefail
X="sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000"
if [ -n "$REL" ]; then fresh_release $T $ALIAS $REL; else fresh_official $T $ALIAS; fi
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
  out=$( { echo "FRESH_MINT=$FRESH_MINT T=$T P=$P ALIAS=$ALIAS STAMP=$STAMP"; cat "$HERE/fresh_pair_lib.sh"; cat <<'EOF'; } | ssh "$HOST" "bash -s" 2>&1
set -euo pipefail
fresh_official $T $ALIAS
fresh_official $P $ALIAS-p
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

if [ "$MODE" = continuity ] || [ "$MODE" = failures ]; then
  CAND=$PREVIOUS
  [ -f "$CAND" ] || finish FAIL "the candidate build $CAND is not a file"
  scp -q "$CAND" "$HOST:/tmp/$T-cand"
  if [ "$MODE" = failures ]; then [ -f "$WRONG" ] || finish FAIL "the wrong-platform binary $WRONG is not a file"; scp -q "$WRONG" "$HOST:/tmp/$T-wrong"; fi
  out=$( { echo "FRESH_MINT=$FRESH_MINT T=$T ALIAS=$ALIAS MODE=$MODE"; echo "$SHIM"; cat "$HERE/fresh_pair_lib.sh"; cat <<'EOF'; } | ssh "$HOST" "bash -s" 2>&1
set -euo pipefail
X="sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000"
fresh_official $T $ALIAS
sudo -n docker cp /tmp/$T-ds.tgz $T:/home/pbt/ds.tgz
sudo -n docker cp /tmp/$T-cand $T:/home/pbt/cand
[ "$MODE" = failures ] && sudo -n docker cp /tmp/$T-wrong $T:/home/pbt/wrong
sudo -n docker exec $T chown pbt:pbt /home/pbt/ds.tgz /home/pbt/cand
[ "$MODE" = failures ] && sudo -n docker exec $T chown pbt:pbt /home/pbt/wrong
sudo -n docker exec $T chmod 755 /home/pbt/cand
$X $T bash -c 'mkdir -p ~/ds && tar xzf ~/ds.tgz -C ~/ds'
for i in $(seq 1 24); do $X $T timeout 40 /home/pbt/.local/bin/pb exec $ALIAS -- true >/dev/null 2>&1 && break; sleep 5; done
if [ "$MODE" = continuity ]; then run="python3 update_continuity.py $ALIAS /home/pbt/cand"; else run="python3 update_failures.py $ALIAS /home/pbt/cand /home/pbt/wrong"; fi
if res=$(echo x | $X $T bash -c "cd ~/ds && $run" 2>&1); then echo UPDATE_OK; else echo UPDATE_FAILED; fi
echo "$res" | tail -n 3
EOF
)
  MACHINE_IDS=$(sed -n 's/^MACHINE_ID=\(machine_[0-9a-f-]*\)$/\1/p' <<<"$out" | tr '\n' ' ')
  rm_extra="rm -f /tmp/$T-cand /tmp/$T-wrong"; ssh "$HOST" "$rm_extra" || true
  grep -qx UPDATE_OK <<<"$out" && finish PASS "official install: $MODE check against $(basename "$CAND") passed"
  finish FAIL "$(grep -v "^MACHINE_ID=\|^OFFICIAL_" <<<"$out" | tail -n 3 | cut -c1-300 | tr "\n" " ")"
fi

# history: the target starts on the previous official release, the peer on the official Latest.
DEV=$WRONG  # history: the optional build to update to
[ -z "$DEV" ] || scp -q "$DEV" "$HOST:/tmp/$T-dev" || finish FAIL "could not copy the build to $HOST"
out=$( { echo "FRESH_MINT=$FRESH_MINT T=$T P=$P ALIAS=$ALIAS PREVIOUS=$PREVIOUS DEVFILE=${DEV:+/tmp/$T-dev}"; echo "$SHIM"; cat "$HERE/fresh_pair_lib.sh"; cat <<'EOF'; } | ssh "$HOST" "bash -s" 2>&1
set -euo pipefail
X="sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000"
if [ -n "$PREVIOUS" ]; then fresh_release $T $ALIAS $PREVIOUS; else fresh_official $T $ALIAS; fi
fresh_official $P $ALIAS-p
sudo -n docker cp /tmp/$T-ds.tgz $T:/home/pbt/ds.tgz
$X $T bash -c 'mkdir -p ~/ds && tar xzf ~/ds.tgz -C ~/ds'
# the update is what is judged here (first exec after a pair is the exec mode's job): wait until the pair is usable
for i in $(seq 1 24); do $X $T timeout 40 /home/pbt/.local/bin/pb exec $ALIAS-p -- true >/dev/null 2>&1 && break; sleep 5; done
if [ -n "$DEVFILE" ]; then
  sudo -n docker cp $DEVFILE $T:/home/pbt/final-pb && $X $T chmod 755 /home/pbt/final-pb
  UPDATE="--from /home/pbt/final-pb"
else
  $X $T bash -c "/home/pbt/.local/bin/pb update check --json >/dev/null && /home/pbt/.local/bin/pb update download --json >/dev/null" || true
  UPDATE="--approve"
fi
if echo x | $X $T bash -c "cd ~/ds && python3 update_survival.py $ALIAS $ALIAS-p --terminal --mib 400 $UPDATE"; then echo HISTORY_OK; else echo HISTORY_FAILED; fi
echo "FINAL_$T=$($X $T /home/pbt/.local/bin/pb --version | sed -n 's/.*Version //p' | head -1)"
EOF
)
MACHINE_IDS=$(sed -n 's/^MACHINE_ID=\(machine_[0-9a-f-]*\)$/\1/p' <<<"$out" | tr '\n' ' ')
official=$(sed -n 's/^OFFICIAL_[^=]*=//p' <<<"$out" | head -1) final=$(sed -n 's/^FINAL_[^=]*=//p' <<<"$out" | head -1)
grep -qx HISTORY_OK <<<"$out" && finish PASS "official '$official' updated in place to '$final' with a preview, a tunnel, a held terminal and a transfer running; all survived"
finish FAIL "$(grep -v "^MACHINE_ID=\|^OFFICIAL_\|^FINAL_" <<<"$out" | tail -n 3 | cut -c1-300 | tr "\n" " ")"
