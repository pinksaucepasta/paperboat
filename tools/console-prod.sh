#!/bin/sh
# Redeploys the production console (its own host, behind that host's Caddy) to origin/main.
#
# The build runs on a GitHub runner (.github/workflows/console-build.yml: Go for the terminal WASM, then next build) into
# the slot the host is NOT serving from; the host installs the production dependencies for its own platform and starts
# the new slot. A new slot that does not answer leaves the previous build running.
# Usage: tools/console-prod.sh   (any worktree; PB_CONSOLE_PROD_HOST overrides the ssh target, the `console_host` target)
set -eu
host=${PB_CONSOLE_PROD_HOST:-$(python3 "$(dirname "$0")/device-smoke/dev_targets.py" console_host)}
[ -n "$host" ] || { echo "configure console_host in local development targets" >&2; exit 2; }
command -v gh >/dev/null || { echo "console-prod: gh is required" >&2; exit 2; }
# One deploy at a time: two overlapping runs would pick the same free slot and the second would delete it while serving.
mkdir -p "$HOME/.config/paperboat-console"
exec 9>"$HOME/.config/paperboat-console/prod.lock"
flock -w 1800 9 || { echo "console-prod: another deploy is still running" >&2; exit 1; }
dir=/opt/paperboat-console
here=$(cd "$(dirname "$0")" && pwd)
repo=$(git -C "$here" rev-parse --show-toplevel)
git -C "$repo" fetch -q origin main
want=$(git -C "$repo" rev-parse FETCH_HEAD)
old=$(ssh "$host" "sed -n 's/^NEXT_DIST_DIR=//p' $dir/slot.env 2>/dev/null" || true)
case $old in .next-a) new=.next-b ;; *) new=.next-a ;; esac
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
# The browser-public build values (NEXT_PUBLIC_*) of the host's console.env; nothing else of it leaves the host.
public() { ssh "$host" "sed -n 's/^$1=//p' /etc/paperboat-console/console.env" | tr -d '"'"'"; }
nonce="$(printf %.7s "$want")-$(date +%s)-$$"
gh workflow run console-build.yml --ref main -f commit="$want" -f slot="$new" -f nonce="$nonce" \
  -f posthog_key="$(public NEXT_PUBLIC_POSTHOG_KEY)" -f posthog_host="$(public NEXT_PUBLIC_POSTHOG_HOST)" \
  -f sentry_dsn="$(public NEXT_PUBLIC_SENTRY_DSN)"
run=
for _ in $(seq 60); do
  run=$(gh run list --workflow console-build.yml --limit 20 --json databaseId,displayTitle \
    --jq "first(.[] | select(.displayTitle == \"Console build $nonce\") | .databaseId)" 2>/dev/null || true)
  [ -n "$run" ] && break
  sleep 5
done
[ -n "$run" ] || { echo "console-prod: the dispatched build did not appear; nothing was deployed" >&2; exit 1; }
echo "console-prod: building ${want%${want#???????}} into $new (run $run)"
timeout 1800 gh run watch "$run" --exit-status >/dev/null || { echo "console-prod: build run $run failed; $old keeps running" >&2; exit 1; }
gh run download "$run" -n console-build -D "$work"
mkdir "$work/c" && tar -xzf "$work/console-build.tgz" -C "$work/c"
cd "$work/c"
ssh "$host" "rm -rf $dir/$new"
rsync -a --delete --exclude '.next-*' --exclude node_modules --exclude slot.env package.json bun.lock bunfig.toml next.config.ts patches public "$host:$dir/"
rsync -a "$new/" "$host:$dir/$new/"
rsync -a "$here/../deploy/paperboat-console.service" "$host:/etc/systemd/system/paperboat-console.service"
ssh "$host" "set -eu
  cd $dir
  chown -R pbconsole:pbconsole .
  runuser -u pbconsole -- env HOME=$dir bun install --production --frozen-lockfile
  systemctl daemon-reload
  systemctl enable paperboat-console >/dev/null 2>&1
  point() { printf 'NEXT_DIST_DIR=%s\n' \"\$1\" > slot.env; systemctl restart paperboat-console; }
  point $new
  for _ in \$(seq 30); do
    curl -fsS -o /dev/null --max-time 5 http://172.18.0.1:3100/ 2>/dev/null && { echo 'console-prod: serving $new at ${want%${want#???????}}'; exit 0; }
    sleep 1
  done
  echo 'console-prod: $new did not answer; back on ${old:-nothing}' >&2
  [ -n '$old' ] && point '$old'
  exit 1"
