#!/bin/sh
# usage: rehearse-migrations.sh <image tag> <commit>   Migrates a restored copy of the newest backup with the server image
# <tag>, in a scratch Postgres, and prints the migrate time and the row-count diff. Refuses an image that was not built
# from <commit> (its revision label), so a stale tag can never be rehearsed. Runs on the server host from /opt/paperboat.
set -eu
cd /opt/paperboat
. ./.env
scratch_start() { # a throwaway Postgres (PAPERBOAT_POSTGRES_IMAGE, the production major version); prints its name
  name=pb-scratch-$$
  docker run -d --name "$name" -e POSTGRES_PASSWORD=scratch -e POSTGRES_DB=paperboat --memory 1536m --memory-swap 1536m \
    "$PAPERBOAT_POSTGRES_IMAGE" >/dev/null
  for _ in $(seq 1 60); do docker exec "$name" pg_isready -U postgres -d paperboat >/dev/null 2>&1 && break; sleep 1; done
  sleep 2
  docker exec "$name" pg_isready -U postgres -d paperboat >/dev/null
  echo "$name"
}
[ "$#" -eq 2 ] || { echo "usage: $0 <image tag> <commit>" >&2; exit 64; }
V=$1 COMMIT=$2
revision=$(docker image inspect "paperboat-server:$V" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}')
[ "$revision" = "$COMMIT" ] || { echo "paperboat-server:$V was built from ${revision:-an unknown commit}, not $COMMIT: a stale tag. Build it again under a new tag; nothing was rehearsed." >&2; exit 1; }
name=$(scratch_start)
trap "docker rm -f $name >/dev/null 2>&1" EXIT
# A fresh dump of the production database (on its own host), taken in the scratch container with the server's DSN
# as libpq variables (rollout-server.py's database_env): nothing secret in argv.
pgenv=$(python3 -c 'import importlib.util as u, shlex, sys; s = u.spec_from_file_location("r", "/opt/paperboat/rollout-server.py"); m = u.module_from_spec(s); s.loader.exec_module(m)
for k, v in m.database_env(open("/opt/paperboat/server.env").read()).items(): print("export %s=%s" % (k, shlex.quote(v)))') ||
  { echo "rehearse: cannot read the database setting from server.env; nothing was rehearsed" >&2; exit 1; }
# A subshell: the production connection settings never stay in this script's environment.
(eval "$pgenv"; docker exec -e PGHOST -e PGPORT -e PGUSER -e PGPASSWORD -e PGDATABASE -e PGSSLMODE \
  -e PGSSLROOTCERT -e PGCONNECT_TIMEOUT "$name" pg_dump --format=custom -f /tmp/r.dump)
echo "dumped the production database"
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
