#!/bin/sh
set -eu
cd "$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
umask 077
backup_dir=${PAPERBOAT_BACKUP_DIR:-./backups}
mkdir -p "$backup_dir"
chmod 700 "$backup_dir"
backup_tmp=$(mktemp "$backup_dir/.database.XXXXXX")
cleanup() {
  result=$?
  trap - EXIT HUP INT TERM
  rm -f "$backup_tmp"
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM
if ! timeout 300 docker compose exec -T postgres pg_dump -U postgres -d paperboat --format=custom > "$backup_tmp"; then
  echo 'Database backup failed; previous backups preserved. Check database health and free disk space.' >&2
  exit 1
fi
[ -s "$backup_tmp" ] || { echo 'Database backup was empty; previous backups preserved.' >&2; exit 1; }
backup_name="$backup_dir/database-$(date -u +%Y%m%dT%H%M%S)-$$.dump"
chmod 600 "$backup_tmp"
mv "$backup_tmp" "$backup_name"
printf '%s\n' 'Database backup completed.'
