#!/bin/sh
# Redeploys the production admin panel (served by the server box's proxy) to origin/main.
#
# Builds a clean checkout of main on this machine (never the shared ~/paperboat tree), ships the build to the
# box, which installs the production dependencies for its own platform and starts the new build in the slot it is
# NOT serving from. A new slot that does not answer leaves the previous build running.
# Usage: tools/admin-prod.sh   (any worktree; PB_ADMIN_PROD_HOST overrides the ssh target)
# One-time setup on the box (not done by this script): the pbadmin user, /opt/paperboat-admin, /etc/paperboat-admin/admin.env,
# and a proxy route from the panel's origin to 172.18.0.1:3101.
set -eu
host=${PB_ADMIN_PROD_HOST:-$(python3 "$(dirname "$0")/device-smoke/dev_targets.py" deployment_host)}
[ -n "$host" ] || { echo "configure the host in local development targets" >&2; exit 2; }
dir=/opt/paperboat-admin
here=$(cd "$(dirname "$0")" && pwd)
repo=$(git -C "$here" rev-parse --show-toplevel)
git -C "$repo" fetch -q origin main
want=$(git -C "$repo" rev-parse FETCH_HEAD)
build=$(mktemp -d)
trap 'rm -rf "$build"' EXIT
git -C "$repo" archive "$want" admin server/docs/openapi.json | tar -x -C "$build"
old=$(ssh "$host" "sed -n 's/^NEXT_DIST_DIR=//p' $dir/slot.env 2>/dev/null" || true)
case $old in .next-a) new=.next-b ;; *) new=.next-a ;; esac
cd "$build/admin"
bun install --frozen-lockfile
NEXT_DIST_DIR=$new bun run build
ssh "$host" "rm -rf $dir/$new"
rsync -a --delete --exclude '.next-*' package.json bun.lock next.config.ts "$host:$dir/"
rsync -a "$new/" "$host:$dir/$new/"
rsync -a "$here/../deploy/paperboat-admin.service" "$host:/etc/systemd/system/paperboat-admin.service"
ssh "$host" "set -eu
  cd $dir
  chown -R pbadmin:pbadmin .
  runuser -u pbadmin -- env HOME=$dir bun install --production --frozen-lockfile
  systemctl daemon-reload
  systemctl enable paperboat-admin >/dev/null 2>&1
  point() { printf 'NEXT_DIST_DIR=%s\n' \"\$1\" > slot.env; systemctl restart paperboat-admin; }
  point $new
  for _ in \$(seq 30); do
    curl -fsS -o /dev/null --max-time 5 http://172.18.0.1:3101/login 2>/dev/null && { echo 'admin-prod: serving $new at ${want%${want#???????}}'; exit 0; }
    sleep 1
  done
  echo 'admin-prod: $new did not answer; back on ${old:-nothing}' >&2
  [ -n '$old' ] && point '$old'
  exit 1"
