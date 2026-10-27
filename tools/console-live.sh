#!/bin/sh
# Updates the live console (~/paperboat/console, served through the Tailscale Funnel) to origin/main.
#
# Fast-forwards the tree, installs dependencies, builds into the slot the server is NOT running from
# (.next-a / .next-b, chosen by the unit's EnvironmentFile), then points the unit at the new slot and
# restarts it. A failed build or a new slot that does not answer leaves the previous build running.
# Usage: tools/console-live.sh   (any worktree; touches only ~/paperboat and the paperboat-console unit)
set -eu
tree=${PB_CONSOLE_TREE:-$HOME/paperboat}
url=${PB_CONSOLE_URL:-http://100.95.70.63:3000}
slot_file=$HOME/.config/paperboat-console/slot.env
old=$(sed -n 's/^NEXT_DIST_DIR=//p' "$slot_file" 2>/dev/null || true)
case $old in .next-a) new=.next-b ;; *) old=.next-b new=.next-a ;; esac
point() { printf 'NEXT_DIST_DIR=%s\n' "$1" > "$slot_file"; systemctl --user restart paperboat-console; }

git -C "$tree" pull -q --ff-only origin main
cd "$tree/console"
bun install --frozen-lockfile
rm -rf "$new"
NEXT_DIST_DIR=$new bun run build || { echo "console-live: build failed; $old keeps running" >&2; exit 1; }

point "$new"
for _ in $(seq 30); do
  curl -fsS -o /dev/null --max-time 5 "$url/" 2>/dev/null && { echo "console-live: serving $new at $(git -C "$tree" rev-parse --short HEAD)"; exit 0; }
  sleep 1
done
echo "console-live: $new did not answer at $url; back on $old" >&2
point "$old"
exit 1
