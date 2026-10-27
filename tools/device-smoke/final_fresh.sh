#!/bin/bash
# final_fresh.sh up|down HOST STAMP [TEAMMATE_MINT]: the run's own machines for the final re-check, on a Docker host (hp).
#   up    three fresh containers, each installed with the published one-liner (the official Latest) and freshly enrolled in the
#         run's throwaway account (FRESH_MINT: a pb signed in to it, set up by final_recheck.py): final-a-STAMP as fx-a and
#         final-b-STAMP as fx-b (a usable pair: fx-a runs a command on fx-b), final-c-STAMP as fx-c, stopped at once so the
#         account has an offline machine. Prints MACHINE_ID= and OFFICIAL_<container>=<version> lines; exit 1 if the pair is not usable.
#         With TEAMMATE_MINT (a pb signed in to a SECOND throwaway account) a fourth container final-d-STAMP as fx-d is installed
#         and enrolled in that account: the teammate's machine (team member, F9 accessor).
#   down  removes the containers. Their machines go with the throwaway account.
set -uo pipefail
MODE=${1:?up|down} HOST=${2:?host} STAMP=${3:?stamp} MINT2=${4:-}
HERE=$(cd "$(dirname "$0")" && pwd)
SHIM=; [ "${SMOKE_CT_DOCKER:-}" = docker ] && SHIM='sudo() { shift; "$@"; }'
if [ "$MODE" = down ]; then
  ssh "$HOST" "${SMOKE_CT_DOCKER:-sudo -n docker} rm -f final-a-$STAMP final-b-$STAMP final-c-$STAMP final-d-$STAMP >/dev/null 2>&1; true"
  exit 0
fi
: "${FRESH_MINT:?the pb of the fresh throwaway account}"
{ echo "FRESH_MINT=$FRESH_MINT MINT2=$MINT2 STAMP=$STAMP"; echo "$SHIM"; cat "$HERE/fresh_pair_lib.sh"; cat <<'REMOTE'; } | ssh "$HOST" "bash -s" 2>&1
set -euo pipefail
fresh_official final-a-$STAMP fx-a
fresh_official final-b-$STAMP fx-b
fresh_official final-c-$STAMP fx-c
[ -z "$MINT2" ] || FRESH_MINT=$MINT2 fresh_official final-d-$STAMP fx-d
sudo -n docker stop final-c-$STAMP >/dev/null
X="sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000"
ready=
for i in $(seq 1 24); do $X final-a-$STAMP timeout 40 /home/pbt/.local/bin/pb exec fx-b -- true >/dev/null 2>&1 && ready=1 && break; sleep 5; done
[ -n "$ready" ] || { echo "the fresh pair is not usable: fx-a cannot run a command on fx-b"; exit 1; }
# the stopped machine shows offline only after the server's heartbeat timeout
for i in $(seq 1 60); do
  $X final-a-$STAMP /home/pbt/.local/bin/pb machine list --json | python3 -c 'import json,sys;sys.exit(0 if any(m["alias"]=="fx-c" and not m["online"] for m in json.load(sys.stdin)["machines"]) else 1)' && exit 0
  sleep 5
done
echo "fx-c did not show offline within 5 minutes"; exit 1
REMOTE
