#!/usr/bin/env bash
# The team-scoped cross-OS rows on the two accounts and paid team of xos_team.py, run through it:
#   xos_team.py run BUILD_DIR -- xos_team_rows.sh [PART...]     PART = private-end | team | env (default all, in this order)
# Every part runs even when an earlier one failed; exit status = number of failed parts. env runs last: its revoke removes
# the member from the team. CX_OUT is the directory for the logs and result files.
#   private-end  X-19 team part: held member flows end on grant revoke and on the team setting, six OS pairs
#   team         X-21: sharing, admission, exec, terminal and session roles, files, tunnel/preview management, private access,
#                revoke/unshare, member removal and suspension, six OS pairs
#   env          X-12 team part: a team scope reaches L, M and W, from the owner and a granted member, and is withdrawn on revoke
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
: "${CX_OUT:?}" "${XPE_TEAM:?run through xos_team.py run}"
mkdir -p "$CX_OUT" && export SMOKE_RESULTS_DIR="$CX_OUT"
failed=0 names=()
step() { # NAME TIMEOUT command...
  local name=$1 limit=$2 rc=0; shift 2
  echo "--- team/$name started $(date -u +%H:%M:%S)"
  (cd "$HERE" && timeout "$limit" "$@") >"$CX_OUT/team-$name.log" 2>&1 || rc=$?
  if [ $rc = 0 ]; then echo "PASS team/$name"; else
    echo "FAIL team/$name (exit $rc) log $CX_OUT/team-$name.log"; failed=$((failed + 1)); names+=("$name")
    grep -E '^FAIL|"status": "FAIL"|Traceback|Error:' "$CX_OUT/team-$name.log" | cut -c1-300 | head -60
  fi
}
parts=("$@"); [ $# != 0 ] || parts=(private-end team env)
for part in "${parts[@]}"; do
  case $part in
    private-end) step private-end 1800 python3 -u xplat_team_private_end.py ;;
    team) step team 5400 python3 -u xplat_team.py ;;
    env) step env 2400 python3 -u xplat_envteam.py --devices L,M,W ;;
    *) echo "unknown part $part" >&2; exit 2 ;;
  esac
done
if [ $failed = 0 ]; then echo "PASS commands-xos team ${parts[*]}"; else echo "FAIL commands-xos team: ${names[*]}"; fi
exit $failed
