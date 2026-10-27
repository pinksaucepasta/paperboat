#!/bin/sh
# usage: rehearse.sh [dump]   Runs the whole rebuild (migrate an EMPTY database with the live server image, import
# config + fixtures) in a scratch container against a restored copy of prod, and compares. Prod is only read.
. "$(dirname "$0")/lib.sh"
dump=${1:-$(ls -t backups/database-*.dump | head -1)}
image=$(docker inspect -f '{{.Config.Image}}' "$(python3 -c 'import json;print(json.load(open("server-rollout.json"))["active"])')")
name=$(scratch_start)
out=$(mktemp -d /tmp/pb-rehearse.XXXXXX)
[ -n "${KEEP:-}" ] || trap 'docker rm -f "$name" >/dev/null 2>&1; rm -rf "$out"' EXIT
psqls() { docker exec "$name" psql -U postgres "$@"; }
psqls -d postgres -c "CREATE DATABASE src" -c "CREATE DATABASE dst" >/dev/null
docker cp "$dump" "$name:/tmp/r.dump"
docker exec "$name" pg_restore -U postgres -d src --no-owner --exit-on-error /tmp/r.dump
sh "$(dirname "$0")/export-config.sh" "$dump" "$out/x" | tail -n 3
echo "migrating an empty database with $image"
docker network connect --alias pb-scratch paperboat_default "$name"
PAPERBOAT_SERVER_IMAGE="$image" docker compose run --rm --no-deps -e "PAPERBOAT_DATABASE_DSN=postgres://postgres:scratch@pb-scratch:5432/dst?sslmode=disable" migrate 2>&1 | tail -n 2
echo "tables before import: $(psqls -d dst -At -c "select sum(n_live_tup) from pg_stat_user_tables where schemaname='paperboat'")"
echo "scratch kept: $name $out"; sh "$(dirname "$0")/import-config.sh" "$out/x" "$name" dst
echo "REHEARSAL-OK"
