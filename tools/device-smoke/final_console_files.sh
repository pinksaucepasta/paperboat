#!/bin/bash
# final_console_files.sh HOST CT_A CT_B: F6-06 on the live console. CT_A (user pbt, uid 1000, Docker host HOST) sends a
# unique file to CT_B's Inbox; check-files-machine.js then lists it on that machine's Files screen, downloads it (exact
# bytes), deletes it through the row menu and confirm dialog, and checks an offline machine of the account says so.
# Needs ORIGIN, STATE, PW_CHROMIUM, PW_MODULE (see console/scripts/run-console-check.mjs) and one offline machine on
# the account. Team-request approval on the screen is not covered here.
set -uo pipefail
HOST=${1:?docker host} A=${2:?container A} B=${3:?container B}
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
DOCKER=${SMOKE_CT_DOCKER:-docker}
X() { local ct=$1; shift; ssh -o BatchMode=yes "$HOST" "$DOCKER exec -i -u pbt -w /home/pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 $ct bash -lc $(printf '%q' "$*")"; }
STAMP=$(date +%s) NAME=final-files-$STAMP.txt CONTENT=FINAL_FILES_$STAMP
LIST=$(X "$A" '.local/bin/pb machine list --json') || { echo "FAIL files: could not list machines"; exit 1; }
SELF_B=$(X "$B" "python3 -c 'import json;print(json.load(open(\".local/state/paperboat/runtime/machine-registration.json\"))[\"machine_id\"])'") || { echo "FAIL files: no machine id in $B"; exit 1; }
PEER=$(python3 -c 'import json,sys;l=json.loads(sys.argv[1])["machines"];print(*[m["alias"] for m in l if m["id"]==sys.argv[2]])' "$LIST" "$SELF_B")
OFFLINE=$(python3 -c 'import json,sys;l=json.loads(sys.argv[1])["machines"];o=[m["id"] for m in l if not m["online"]];print(o[0] if o else "")' "$LIST")
[ -n "$PEER" ] && [ -n "$OFFLINE" ] || { echo "FAIL files: need container B's alias and one offline machine on the account"; exit 1; }
X "$A" "printf %s $CONTENT > $NAME && .local/bin/pb send $NAME --to $PEER --json >/dev/null && rm -f $NAME" || { echo "FAIL files: could not send the fixture to $PEER"; exit 1; }
exec node "$ROOT/console/scripts/run-console-check.mjs" "$ROOT/console/scripts/check-files-machine.js" /files \
  "{\"__online\":\"$SELF_B\",\"__offline\":\"$OFFLINE\",\"__file\":\"$NAME\",\"__content\":\"$CONTENT\",\"__delete\":true}"
