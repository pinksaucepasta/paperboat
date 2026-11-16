#!/bin/sh
# Redeploys the production console (served by the server box's proxy) to origin/main.
#
# console-live.sh builds main on this machine; the build output is shipped to the box, which installs the
# production dependencies for its own platform and starts the new build in the slot it is NOT serving from.
# A new slot that does not answer leaves the previous build running.
# Usage: tools/console-prod.sh   (any worktree; PB_CONSOLE_PROD_HOST overrides the ssh target)
set -eu
host=${PB_CONSOLE_PROD_HOST:-root@coolify}
dir=/opt/paperboat-console
here=$(cd "$(dirname "$0")" && pwd)
"$here/console-live.sh"
# Hold console-live's lock so no other deploy flips or deletes the slot being shipped.
exec 9>"$HOME/.config/paperboat-console/live.lock"
flock -w 1800 9 || { echo "console-prod: another deploy is still running" >&2; exit 1; }
tree=${PB_CONSOLE_TREE:-$HOME/paperboat}
slot=$(sed -n 's/^NEXT_DIST_DIR=//p' "$HOME/.config/paperboat-console/slot.env")
head=$(git -C "$tree" rev-parse HEAD)
[ "$(cat "$HOME/.config/paperboat-console/built.commit")" = "$head" ] || { echo "console-prod: the live build is not main" >&2; exit 1; }
old=$(ssh "$host" "sed -n 's/^NEXT_DIST_DIR=//p' $dir/slot.env 2>/dev/null" || true)
case $old in .next-a) new=.next-b ;; *) new=.next-a ;; esac
cd "$tree/console"
ssh "$host" "rm -rf $dir/$new"
rsync -a --delete --exclude '.next-*' package.json bun.lock bunfig.toml next.config.ts patches public "$host:$dir/"
rsync -a "$slot/" "$host:$dir/$new/"
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
    curl -fsS -o /dev/null --max-time 5 http://172.18.0.1:3100/ 2>/dev/null && { echo 'console-prod: serving $new at ${head%${head#???????}}'; exit 0; }
    sleep 1
  done
  echo 'console-prod: $new did not answer; back on ${old:-nothing}' >&2
  [ -n '$old' ] && point '$old'
  exit 1"
