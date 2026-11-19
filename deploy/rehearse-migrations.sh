#!/bin/sh
# usage: rehearse-migrations.sh <image tag> <commit>   Migrates a restored copy of the newest backup with the server image
# <tag>, in a scratch Postgres, and prints the migrate time and the row-count diff. Refuses an image that was not built
# from <commit> (its revision label), so a stale tag can never be rehearsed. Runs on the server host from /opt/paperboat.
set -eu
. /opt/paperboat/final-recheck/lib.sh
cd /opt/paperboat
[ "$#" -eq 2 ] || { echo "usage: $0 <image tag> <commit>" >&2; exit 64; }
V=$1 COMMIT=$2
revision=$(docker image inspect "paperboat-server:$V" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}')
[ "$revision" = "$COMMIT" ] || { echo "paperboat-server:$V was built from ${revision:-an unknown commit}, not $COMMIT: a stale tag. Build it again under a new tag; nothing was rehearsed." >&2; exit 1; }
dump=$(ls -t backups/database-*.dump | head -1)
echo "dump $dump"
name=$(scratch_start)
trap "docker rm -f $name >/dev/null 2>&1" EXIT
docker cp "$dump" "$name:/tmp/r.dump"
docker cp counts211.sql "$name:/tmp/counts.sql"
docker exec "$name" pg_restore -U postgres -d paperboat --no-owner --exit-on-error /tmp/r.dump
P="docker exec $name psql -U postgres -d paperboat"
echo "goose before: $($P -At -c "select max(version_id) from paperboat.goose_db_version")"
$P -f /tmp/counts.sql > /tmp/counts-before.txt
docker network connect --alias pb-scratch paperboat_default "$name"
s=$(date +%s)
PAPERBOAT_SERVER_IMAGE=paperboat-server:$V docker compose run --rm --no-deps -e "PAPERBOAT_DATABASE_DSN=postgres://postgres:scratch@pb-scratch:5432/paperboat?sslmode=disable" migrate 2>&1 | tail -n 60 | cut -c1-250
echo "migrate seconds: $(( $(date +%s) - s ))"
echo "goose after: $($P -At -c "select max(version_id) from paperboat.goose_db_version")"
echo "not-valid constraints left: $($P -At -c "select count(*) from pg_constraint where not convalidated")"
$P -f /tmp/counts.sql > /tmp/counts-after.txt
echo "=== count diff"; diff /tmp/counts-before.txt /tmp/counts-after.txt || true
echo REHEARSAL-DONE
