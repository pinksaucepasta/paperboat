#!/bin/sh
# Runs on the database host (systemd paperboat-backup.timer, daily): a mode-0600 custom-format dump of paperboat,
# published atomically into /var/backups/paperboat. A failed or empty dump keeps every previous backup; dumps older
# than 7 days are removed only after a new one succeeded.
set -eu
umask 077
backup_dir=${PAPERBOAT_BACKUP_DIR:-/var/backups/paperboat}
mkdir -p "$backup_dir"
chown postgres "$backup_dir"
chmod 700 "$backup_dir"
backup_tmp=$(mktemp "$backup_dir/.database.XXXXXX")
chown postgres "$backup_tmp"
trap 'rm -f "$backup_tmp"' EXIT
trap 'exit 130' HUP INT TERM
if ! timeout 1800 runuser -u postgres -- pg_dump -d paperboat --format=custom -f "$backup_tmp"; then
  echo 'Database backup failed; previous backups preserved. Check database health and free disk space.' >&2
  exit 1
fi
[ -s "$backup_tmp" ] || { echo 'Database backup was empty; previous backups preserved.' >&2; exit 1; }
mv "$backup_tmp" "$backup_dir/database-$(date -u +%Y%m%dT%H%M%SZ).dump"
find "$backup_dir" -maxdepth 1 -type f -name 'database-*.dump' -mtime +6 -delete
printf '%s\n' 'Database backup completed.'
