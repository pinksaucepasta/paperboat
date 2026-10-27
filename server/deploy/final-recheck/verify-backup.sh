#!/bin/sh
# usage: verify-backup.sh <dump>   (default: newest in backups/)
# Proves a backup is restorable: restores it into a scratch Postgres container (same image as prod, tmpfs, removed
# on exit), then compares row counts with the live database. Exit 1 on any restore error or key-table mismatch.
. "$(dirname "$0")/lib.sh"
dump=${1:-$(ls -t backups/database-*.dump | head -1)}
[ -s "$dump" ] || { echo "no dump: $dump" >&2; exit 1; }
name=$(scratch_start)
trap 'docker rm -f "$name" >/dev/null 2>&1' EXIT
docker cp "$dump" "$name:/tmp/verify.dump"
docker exec "$name" pg_restore -U postgres -d paperboat --no-owner --exit-on-error /tmp/verify.dump
bad=0
for t in $KEY_TABLES goose_db_version; do
  live=$(pg -At -c "select count(*) from paperboat.$t")
  copy=$(docker exec "$name" psql -U postgres -d paperboat -At -c "select count(*) from paperboat.$t")
  if [ "$live" != "$copy" ]; then echo "MISMATCH $t live=$live restored=$copy"; bad=1; else echo "ok $t $copy"; fi
done
tables=$(docker exec "$name" psql -U postgres -d paperboat -At -c "select count(*) from information_schema.tables where table_schema='paperboat' and table_type='BASE TABLE'")
echo "restored $tables tables from $dump ($(stat -c %s "$dump") bytes)"
[ "$bad" = 0 ] && echo "BACKUP-VERIFIED $dump"
exit "$bad"
