#!/bin/sh
# usage: rehearse.sh [dump]   Runs the whole rebuild (migrate an EMPTY database with the live server image, import the
# config rows) in a scratch container against a restored copy of prod, then runs final_fixtures.py against it for two
# fresh accounts (create, repeat, teardown). Prod is only read.
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
# per-run fixtures: two accounts that signed in once, a paid team for the first, the second as member, teardown
psqls -d dst -v ON_ERROR_STOP=1 -q -c "INSERT INTO paperboat.users(id,workos_subject,primary_email,canonical_email,email_verified,display_name,status) VALUES ('user_rehearse_a','ws_a','rehearse-a@example.test','rehearse-a@example.test',true,'A','active'),('user_rehearse_b','ws_b','rehearse-b@example.test','rehearse-b@example.test',true,'B','active')"
export FINAL_FIXTURES_PSQL="docker exec -i $name psql -U postgres -d dst"
ff="python3 $(dirname "$0")/final_fixtures.py --email rehearse-a@example.test --wait 5"
$ff --member rehearse-b@example.test
$ff --member rehearse-b@example.test
team=$(psqls -d dst -At -c "select team_id from paperboat.teams where owner_account='user_rehearse_a'")
echo "team $team paid=$(psqls -d dst -At -c "select paperboat.team_paid_entitled('$team')") active_members=$(psqls -d dst -At -c "select count(*) from paperboat.team_members where team_id='$team' and active")"
python3 "$(dirname "$0")/final_fixtures.py" --email rehearse-a@example.test --teardown
echo "after teardown active_members=$(psqls -d dst -At -c "select count(*) from paperboat.team_members where team_id='$team' and active") teams_owned=$(psqls -d dst -At -c "select count(*) from paperboat.teams where owner_account='user_rehearse_a'")"
echo "REHEARSAL-OK"
