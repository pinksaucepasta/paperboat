#!/bin/bash
# run.sh: the quick all-rows `pb update` run on a GitHub-hosted runner (the .github/workflows/quick-update.yml job).
# The runner is the target (user runner, real systemd, like a VM); the controller is a systemd container beside it.
# Builds QV_SRC's HEAD twice (baseline dev-<sha8>-gh<run>-<n>, candidate <same>-r1), pairs both under the throwaway prefix
# lanegapgh<run>x<n>-, runs features.py (held CLI + browser terminals, exec, ssh, send, preview, tunnel) and
# prints the per-feature table. Env: GITHUB_RUN_ID, QV_N (matrix index), QV_SERVER, QV_WEBTOKEN (JSON file with the
# API token secret, never printed), QV_OUT (result dir), PW_MODULE/PW_CHROMIUM/PB_CONSOLE_ORIGIN (browser row; NO_WEB=1 skips).
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd); root=$(cd "$here/../.." && pwd)
: "${QV_SRC:?}" "${GITHUB_RUN_ID:?}" "${QV_SERVER:?}" "${QV_WEBTOKEN:?}" "${QV_OUT:?}"
export QV_VM=local QV_PREFIX=lanegapgh${GITHUB_RUN_ID}x${QV_N:-0}- QV_STATE=${RUNNER_TEMP:-/tmp}/qu-state
export QV_TARGET=${QV_PREFIX}target QV_CTL=${QV_PREFIX}ctl
export XDG_RUNTIME_DIR=/run/user/$(id -u); export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
sha=$(git -C "$QV_SRC" rev-parse HEAD); V=dev-${sha:0:8}-gh$GITHUB_RUN_ID-${QV_N:-0}
mkdir -p ~/gap5 "$QV_OUT"

# Host prep: a user systemd for the runner user (the daemon's user units), inotify headroom as on the VMs.
sudo loginctl enable-linger "$(id -un)"; sudo systemctl start "user@$(id -u).service"
sudo sysctl -q fs.inotify.max_user_instances=1024
docker build -q -t gap5-systemd -f "$root/tools/device-smoke/ubuntu24-systemd.Dockerfile" "$root/tools/device-smoke" >/dev/null

# Builds: linux/amd64, from the checkout under test (QV_SRC); the harness (this directory) is the dispatched ref's.
for pair in "baseline:$V" "candidate:$V-r1"; do
 ld="-X github.com/pinksaucepasta/paperboat/internal/buildinfo.Version=${pair#*:} -X github.com/pinksaucepasta/paperboat/internal/buildinfo.Commit=$sha -X github.com/pinksaucepasta/paperboat/internal/buildinfo.ProtocolVersion=1 -X github.com/pinksaucepasta/paperboat/internal/buildinfo.DefaultServerURL=$QV_SERVER -X github.com/pinksaucepasta/paperboat/internal/buildinfo.DefaultReleaseURL=$QV_SERVER"
 (cd "$QV_SRC/cli" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$ld" -o ~/gap5/${pair%%:*} ./cmd/pb)
done
file ~/gap5/baseline | grep -q "ELF 64-bit.*x86-64"
chmod 755 ~/gap5/baseline ~/gap5/candidate

# No-leftovers check on the target after every install or update; a non-empty "remaining" fails the run.
inv=0; : > "$QV_OUT/inv-rows"
INV() { sh "$root/tools/device-smoke/install_inventory_check.sh" > "$QV_OUT/inv-$1.json" 2>&1 || true; printf '{"container_pids":[%s]}\n' "$(sudo docker top "$QV_CTL" -eo pid | tail -n +2 | tr -s ' \n' , | sed 's/^,//;s/,$//')" >> "$QV_OUT/inv-$1.json"; python3 -I "$here/inv_row.py" "$1" "$QV_OUT/inv-$1.json" >> "$QV_OUT/inv-rows" || inv=1; }
python3 -I "$here/setup.py" ~/gap5/baseline
INV after_baseline
cp "$here/pt.sh" ~/gap5/pt.sh; chmod 755 ~/gap5/pt.sh
read -r preview tunnel < <(~/gap5/pt.sh start | tail -1)
[ -n "${tunnel:-}" ] || { echo "run.sh: preview/tunnel endpoints did not come up" >&2; exit 1; }

rc=0
python3 -I "$here/features.py" "$QV_OUT/run" ~/gap5/candidate "$preview" "$tunnel" > "$QV_OUT/features.out" 2>&1 || rc=$?
~/gap5/pt.sh stop >/dev/null 2>&1 || true
INV after_update
if [ $rc -eq 0 ]; then { python3 -I "$here/table.py" "$QV_OUT/run"; echo; echo "no-leftovers check (install_inventory_check.sh on the target):"; cat "$QV_OUT/inv-rows"; echo; echo "browser terminal events (phases and errors only, no terminal content):"; head -6 "$QV_OUT/run/web-events.jsonl" 2>/dev/null | cut -c1-300; tail -3 "$QV_OUT/run/web.out" 2>/dev/null | cut -c1-300; } | tee "$QV_OUT/table.txt"
else echo "run.sh: features.py failed rc=$rc" >&2; tail -5 "$QV_OUT/features.out" >&2
  echo "browser terminal events (phases and errors only):" >&2; head -6 "$QV_OUT/run/web-events.jsonl" 2>/dev/null | cut -c1-300 >&2; tail -5 "$QV_OUT/run/web.out" 2>/dev/null | cut -c1-300 >&2; fi
# The artifact is the table and the update output only: diagnostics stay private (the repository is public).
cp "$QV_OUT/run/summary.json" "$QV_OUT/run/update.raw" "$QV_OUT/" 2>/dev/null || true
[ $rc -ne 0 ] || rc=$inv
exit $rc
