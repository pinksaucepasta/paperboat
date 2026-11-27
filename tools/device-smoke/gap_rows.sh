#!/usr/bin/env bash
# gap_rows.sh pair|update|revoke A_SSH A_ALIAS B_SSH B_ALIAS    CHECKLIST rows that have a script but no smoke.sh row,
# in smoke.sh's result-line format. A and B are two disposable Linux machines of the smoke account, each with the
# official release under test installed by the published one-liner and paired as its alias (bash, git, passwordless sudo).
#   pair    nothing on A or B changes for good. Rows run on B with A as the peer.
#   update  updates A back and forth (SMOKE_GAP_OTHER is a different official release); B is the peer. The account
#           needs one free preview and one free tunnel (plan limit on running ones).
#   revoke  LAST: B revokes A's machine record (F3-20), then A shows what a revoked machine says (F2-05).
# Every row runs even after a failure; exit 1 when any failed. SMOKE_GAP_ROWS="a b" runs only those rows. A row over its
# time limit gets SIGINT, so the check still removes the tunnels, previews and tokens it made.
set -uo pipefail
[ $# = 5 ] || { sed -n 2,8p "$0"; exit 2; }
GROUP=$1 A=$2 AN=$3 B=$4 BN=$5
HERE=$(cd "$(dirname "$0")" && pwd)
TEAM=${SMOKE_GAP_TEAM:-paperboat-smoke-paid}
OTHER=${SMOKE_GAP_OTHER:-}
RELEASES=${SMOKE_GAP_RELEASES:-https://github.com/pinksaucepasta/paperboat/releases/download}
LOGS=${SMOKE_RESULTS_DIR:-$HERE/results}/gap; mkdir -p "$LOGS"; chmod 700 "$LOGS"
for name in "$AN" "$BN"; do [[ $name =~ ^(test|smoke|test)-[A-Za-z0-9._-]+$ ]] || { echo "alias '$name' is not a throwaway machine name" >&2; exit 2; }; done
SSH=(ssh -o BatchMode=yes -o ConnectTimeout=20 -o ServerAliveInterval=20)
# PB_SMOKE_ALLOW_REAL_HOME: A and B are the devices under test (winhost.refuse_real_home)
ENVLINE='export PATH=$HOME/.local/bin:$PATH XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-/run/user/$(id -u)} PB_SMOKE_ALLOW_REAL_HOME=1; cd /tmp/pbts &&'
stage() { python3 "$HERE/stage_fixtures.py" "$HERE" | "${SSH[@]}" "$1" 'umask 077; rm -rf /tmp/pbts && mkdir /tmp/pbts && tar -xzf - -C /tmp/pbts'; }
cleanup() { local host; for host in "$A" "$B"; do "${SSH[@]}" -n "$host" 'rm -rf /tmp/pbts /tmp/pb-gap-*' || echo "cleanup of $host failed" >&2; done; }
for host in "$A" "$B"; do stage "$host" || { echo "cannot stage the harness on $host" >&2; exit 2; }; done
trap cleanup EXIT
trap 'exit 130' INT TERM
failed=0
row() { # ID HOST SECONDS STDIN COMMAND
  local id=$1 host=$2 limit=$3 input=$4 command=$5 start=$SECONDS rc=0 detail
  [ -z "${SMOKE_GAP_ROWS:-}" ] || [[ " $SMOKE_GAP_ROWS " == *" $id "* ]] || return 0
  printf '%s\n' "$input" | timeout $((limit + 180)) "${SSH[@]}" "$host" "$ENVLINE timeout -s INT -k 120 $limit $command" >"$LOGS/$id.log" 2>&1 || rc=$?
  sed -i -E "s/(PB_TOKEN='?)[A-Z0-9]{26}/\1<redacted>/g" "$LOGS/$id.log"
  detail=$(grep -aE '^(PASS|FAIL|\{"check"|\{"scenario".*"FAILED")' "$LOGS/$id.log" | grep -aE 'FAIL' | head -2 | tr '\n' ' ')
  [ -n "$detail" ] || detail=$(grep -aE '^(PASS|\{)' "$LOGS/$id.log" | tail -1)
  [ -n "$detail" ] || detail=$(grep -av '[▀▄█]' "$LOGS/$id.log" | tail -1)
  if [ $rc = 0 ]; then printf '%-28s %-5s %6ss  %s\n' "$id" PASS $((SECONDS - start)) "${detail:0:150}"
  else failed=1; printf '%-28s %-5s %6ss  %s\n' "$id" FAIL $((SECONDS - start)) "exit $rc: ${detail:0:220} (log $LOGS/$id.log)"; fi
}
case $GROUP in
  pair)
    row exec-offline-messages "$B" 200 '' 'python3 exec_messages.py'
    row shell-completion "$B" 200 '' "python3 completion_lifecycle.py $AN"
    row bugreport-secrets "$B" 200 '' "python3 bugreport_lifecycle.py $AN"
    row cli-paging "$B" 200 '' 'python3 cli_paging.py'
    row signin-extras "$B" 300 '' "python3 signin_extras.py --team $TEAM"
    row team-commands "$B" 200 '' "python3 team_commands.py --team $TEAM"
    row https-front-desk "$B" 320 '' "python3 https_front_desk.py $AN"
    row config-sync-lifecycle "$B" 1500 '' "python3 config_sync_lifecycle.py $BN"
    # F8-09..F8-12 (the other scenarios of the same script) are the billing-policy-tunnels lane's tunnel-origin row
    row preview-origin "$B" 600 '' 'python3 tunnel_origin_rows.py --pb $HOME/.local/bin/pb --out /tmp/pbts/preview-origin.jsonl --only preview'
    ;;
  update)
    [[ $OTHER =~ ^20[0-9]{2}\.[0-9]{2}\.[0-9]{2}\.(0|[1-9][0-9]*)$ ]] || { echo "update: set SMOKE_GAP_OTHER to a different official release" >&2; exit 2; }
    here=$("${SSH[@]}" -n "$A" '$HOME/.local/bin/pb version --json' | grep -oE '20[0-9]{2}\.[0-9]{2}\.[0-9]{2}\.[0-9]+' | head -1)
    [ -n "$here" ] && [ "$here" != "$OTHER" ] || { echo "update: A must run an official release other than SMOKE_GAP_OTHER (A: ${here:-not a release})" >&2; exit 2; }
    case $("${SSH[@]}" -n "$A" uname -m) in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; *) echo "update: unsupported architecture on $A" >&2; exit 2 ;; esac
    "${SSH[@]}" -n "$A" "set -e; cd /tmp; curl -fsSL --retry 3 -o pb-gap-here $RELEASES/$here/pb-linux-$arch; curl -fsSL --retry 3 -o pb-gap-other $RELEASES/$OTHER/pb-linux-$arch; curl -fsSL --retry 3 -o pb-gap-wrong $RELEASES/$OTHER/pb-darwin-arm64; chmod 700 pb-gap-*" ||
      { echo "update: cannot fetch the release binaries on $A" >&2; exit 2; }
    # each update row needs a candidate other than what A runs now, whatever the rows before it did
    candidate() { if "${SSH[@]}" -n "$A" '$HOME/.local/bin/pb version --json' | grep -qF "\"$OTHER\""; then echo /tmp/pb-gap-here; else echo /tmp/pb-gap-other; fi; }
    # official check/download/status/bundle/tamper/offline reinstall: its published one-liner installs Latest and expects
    # nothing newer, so it runs only when A's release is Latest
    latest=$("${SSH[@]}" -n "$A" "curl -fsSL --max-time 60 -o /dev/null -w '%{url_effective}' ${RELEASES%/download}/latest" | sed 's#.*/tag/##')
    if [ -n "$here" ] && [ "$here" = "$latest" ]; then
      row official-update "$A" 600 x "python3 official_update.py --expect-release $here"
    elif [ -z "${SMOKE_GAP_ROWS:-}" ] || [[ " $SMOKE_GAP_ROWS " == *" official-update "* ]]; then
      failed=1; printf '%-28s %-7s %6ss  %s\n' official-update 'NOT RUN' 0 "release under test ${here:-unknown} is not Latest (${latest:-unknown}): official_update.py needs Latest"
    fi
    row update-failures "$A" 600 x "python3 update_failures.py $BN $(candidate) /tmp/pb-gap-wrong"
    row update-continuity "$A" 600 x "python3 update_continuity.py $BN $(candidate)"
    row update-survival "$A" 900 x "python3 update_survival.py $AN $BN --from $(candidate) --terminal --mib 600"
    ;;
  revoke)
    id=$("${SSH[@]}" -n "$A" 'grep -oE "machine_[0-9a-f-]{36}" $HOME/.local/state/paperboat/runtime/machine-registration.json | head -1')
    [[ $id =~ ^machine_[0-9a-f-]{36}$ ]] || { echo "revoke: $A has no machine registration" >&2; exit 2; }
    row dead-machine-terminal "$B" 300 '' "python3 terminal_dead_machine.py $AN $id"
    row revoked-machine "$A" 400 '' "python3 revoked_machine.py verify $BN"
    ;;
  *) echo "unknown group $GROUP" >&2; exit 2 ;;
esac
exit $failed
