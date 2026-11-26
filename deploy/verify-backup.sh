#!/bin/sh
# Runs on the database host: restores <dump> into a new scratch database and compares its schema with paperboat.
set -eu
[ "$#" = 1 ] && [ -f "$1" ] && [ ! -L "$1" ] || { echo 'Provide one regular database dump file.' >&2; exit 2; }
dump=$1
created=false
cleanup() {
  result=$?
  trap - EXIT HUP INT TERM
  if [ "$created" = true ]; then
    timeout 30 runuser -u postgres -- dropdb --force paperboat_restore_test >/dev/null 2>&1 || true
  fi
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM
cd /
# Refuse an existing database rather than replacing another operator's scratch state.
timeout 30 runuser -u postgres -- createdb paperboat_restore_test
created=true
timeout 600 runuser -u postgres -- pg_restore -d paperboat_restore_test --exit-on-error < "$dump"
query="SELECT count(*) FROM information_schema.tables WHERE table_schema='paperboat'"
source_count=$(timeout 30 runuser -u postgres -- psql -X -d paperboat -Atqc "$query")
restore_count=$(timeout 30 runuser -u postgres -- psql -X -d paperboat_restore_test -Atqc "$query")
[ "$source_count" = "$restore_count" ] || { echo 'Restored schema differs from the current database.' >&2; exit 1; }
query='SELECT max(version_id) FROM paperboat.goose_db_version WHERE is_applied'
source_version=$(timeout 30 runuser -u postgres -- psql -X -d paperboat -Atqc "$query")
restore_version=$(timeout 30 runuser -u postgres -- psql -X -d paperboat_restore_test -Atqc "$query")
[ "$source_version" = "$restore_version" ] || { echo 'Restored migration version differs from the current database.' >&2; exit 1; }
printf '%s\n' 'Backup restore verification PASS.'
