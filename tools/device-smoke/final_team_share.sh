#!/bin/bash
# final_team_share.sh CONTAINER ALIAS TEAM: the precondition of private_access_permission.py, which only flips grants and settings: the owner's
# machine ALIAS (in CONTAINER, reached through the ssh of ctssh/ in PATH) is shared into TEAM, an account the fixtures made a team of.
set -euo pipefail
ssh "$1" "export PATH=\$HOME/.local/bin:\$PATH; set -e
M=\$(pb machine list --json | python3 -c 'import json,sys;print([m[\"id\"] for m in json.load(sys.stdin)[\"machines\"] if m[\"alias\"]==\"$2\"][0])')
G=\$(pb team get $3 --json | python3 -c 'import json,sys;print(json.load(sys.stdin)[\"generation\"])')
pb team machine share $3 \$M --generation \$G --json >/dev/null"
