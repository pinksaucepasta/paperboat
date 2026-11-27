#!/usr/bin/env bash
# selfhost_rows.sh HOST VERSION    CHECKLIST S4-01..S4-05, S4-07, in smoke.sh's result-line format.
# HOST: an SSH name of a disposable Linux machine with Docker and passwordless sudo (a linuxpool runner) on which pbh is
# not installed and TCP 80, 443 and 7277 are free. VERSION: the release under test. The published installer takes the
# newest self-host release; VERSION is the other side of the upgrade-in-place step and the image set that is checked.
# Everything pbh creates is removed again. Every row runs; exit 1 when any failed.
set -uo pipefail
[ $# = 2 ] || { sed -n 2,7p "$0"; exit 2; }
HOST=$1 VERSION=$2
[[ $VERSION =~ ^20[0-9]{2}\.[0-9]{2}\.[0-9]{2}\.(0|[1-9][0-9]*)$ ]] || { echo "bad release version" >&2; exit 2; }
HERE=$(cd "$(dirname "$0")" && pwd); REPO=$(cd "$HERE/../.." && pwd)
PUBLISHED=${SMOKE_SELFHOST_ORIGIN:?set SMOKE_SELFHOST_ORIGIN to the published self-host installer URL, https://get.<release domain>/selfhost}
IMAGES=${SMOKE_SELFHOST_IMAGES:-ghcr.io/pinksaucepasta/paperboat}
LOGS=${SMOKE_RESULTS_DIR:-$HERE/results}/selfhost; mkdir -p "$LOGS"; chmod 700 "$LOGS"
SSH=(ssh -o BatchMode=yes -o ConnectTimeout=20 -o ServerAliveInterval=20 "$HOST")
[[ $HOST =~ ^(linuxpool-[0-9]+|(smoke|test|test)-[A-Za-z0-9._-]+)$ ]] || { echo "'$HOST' is not a throwaway host: these rows remove pbh with its data" >&2; exit 2; }
# refused unless the host answers and has no pbh and no node data: everything removed below is what these rows installed
state=$("${SSH[@]}" 'if command -v pbh >/dev/null || sudo -n test -e /var/lib/paperboat-selfhost; then echo present; else echo absent; fi' </dev/null) || { echo "cannot check $HOST" >&2; exit 2; }
[ "$state" = absent ] || { echo "$HOST already has pbh or node data: not touching it" >&2; exit 2; }
cleanup() { "${SSH[@]}" 'if command -v pbh >/dev/null; then sudo -n pbh uninstall --remove-data --confirm-remove-data >/dev/null 2>&1; fi; rm -rf covf-pbq' </dev/null || echo "cleanup of $HOST failed" >&2; }
trap cleanup EXIT
trap 'exit 130' INT TERM
failed=0
row() { # ID command...
  local id=$1 start=$SECONDS rc=0 detail; shift
  "$@" >"$LOGS/$id.log" 2>&1 </dev/null || rc=$?
  sed -i -E 's/pbh1\.[A-Za-z0-9._-]+/pbh1.<redacted>/g' "$LOGS/$id.log"
  detail=$(grep -E '^(PASS|FAIL)' "$LOGS/$id.log" | tail -2 | tr '\n' ' ')
  if [ $rc = 0 ]; then printf '%-28s %-5s %6ss  %s\n' "$id" PASS $((SECONDS - start)) "${detail:0:150}"
  else failed=1; printf '%-28s %-5s %6ss  %s\n' "$id" FAIL $((SECONDS - start)) "exit $rc: ${detail:-$(tail -1 "$LOGS/$id.log")} (log $LOGS/$id.log)" | cut -c1-300; fi
}

# helper/deploy/qualify-fresh-host.sh names the edge's image flag after the capability (--tunnel-image); pbh calls it
# --edge-image (helper/deploy/README.md), so its upgrade-in-place step always failed. The copy run here corrects that one
# line and refuses to run once the original no longer has it (then use the original).
mkdir -p "$REPO/.build/covf"; QUALIFY=$REPO/.build/covf/qualify.sh
BROKEN='  old_args="$old_args --$capability-image ${ref%:*}:$old_tag"'
FIXED='  flag=$capability; [ "$flag" = tunnel ] \&\& flag=edge; old_args="$old_args --$flag-image ${ref%:*}:$old_tag"'
if grep -qxF -- "$BROKEN" "$REPO/helper/deploy/qualify-fresh-host.sh"; then
  sed 's|^  old_args="\$old_args --\$capability-image \${ref%:\*}:\$old_tag"$|'"$FIXED"'|' "$REPO/helper/deploy/qualify-fresh-host.sh" >"$QUALIFY"
else cp "$REPO/helper/deploy/qualify-fresh-host.sh" "$QUALIFY"; fi

images() { # every service image of VERSION is a multi-arch manifest
  local name arch out rc=0
  for name in server relay edge; do
    out=$("${SSH[@]}" "sudo -n docker manifest inspect $IMAGES/$name:$VERSION") || { echo "FAIL $name:$VERSION: no manifest"; rc=1; continue; }
    for arch in amd64 arm64; do grep -qE "\"architecture\": *\"$arch\"" <<<"$out" || { echo "FAIL $name:$VERSION has no linux/$arch"; rc=1; }; done
  done
  [ $rc != 0 ] || echo "PASS server, relay and edge $VERSION are linux/amd64 and linux/arm64"
  return $rc
}
fresh_host() { DOCKER="sudo docker" PAPERBOAT_QUALIFY_OLD_TAG=$VERSION timeout 900 sh "$QUALIFY" --ssh "$HOST" --both --published "$PUBLISHED"; }
on_host() { # qualify arguments: run on HOST, whose Docker holds the throwaway systemd container
  tar -czf - -C "$REPO" tools/fresh-host.sh -C "$REPO/.build/covf" qualify.sh |
    "${SSH[@]}" 'rm -rf covf-pbq && mkdir -p covf-pbq/.build/covf && tar -xzf - -C covf-pbq && mv covf-pbq/qualify.sh covf-pbq/.build/covf/' &&
    "${SSH[@]}" "cd covf-pbq && DOCKER='sudo docker' PAPERBOAT_QUALIFY_OLD_TAG=$VERSION timeout 900 sh .build/covf/qualify.sh $* --published $PUBLISHED"
}
commands() { "${SSH[@]}" "sh -s -- $PUBLISHED" <"$HERE/selfhost_commands.sh"; }

row selfhost-images images
row selfhost-install:ubuntu fresh_host
row selfhost-install:debian12 on_host --base debian:12 --both
row selfhost-refusal:ubuntu20 on_host --base ubuntu:20.04 --expect-refusal
row selfhost-commands commands
exit $failed
