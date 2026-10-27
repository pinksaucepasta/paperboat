#!/bin/sh
# usage: wipe-and-rebuild.sh WIPE-PROD-DATABASE
# The final Linux re-check's clean slate (user decision 2026-10-27). Keeps: every secret/config file, images, WorkOS and
# Stripe settings, both managed edges' hosts, and the rows an operator provisioned (edge nodes, installations, usage
# keys, operator account, billing products, update policy, platform wildcard certificates, WorkOS cursor), so the
# edges keep working untouched. Wipes all user data; the final re-check then makes its own throwaway accounts and
# gives each its paid team with tools/device-smoke/final_fixtures.py.
# Order: stop servers -> fresh backup -> verified restorable -> export the config rows from it -> drop/recreate ->
# migrate with the live server image -> import -> start -> health. After the drop, any failure restores the backup.
. "$(dirname "$0")/lib.sh"
set +e # every step is checked by hand: after the drop a failure must restore the backup, not just exit
[ "${1:-}" = "WIPE-PROD-DATABASE" ] || { echo "refusing: pass WIPE-PROD-DATABASE to confirm" >&2; exit 2; }
set -o noclobber; echo "final-recheck-wipe-$(date -u +%FT%TZ)" > deploy.lock; set +o noclobber
active=$(python3 -c 'import json;print(json.load(open("server-rollout.json"))["active"])')
retired=$(python3 -c 'import json;print(" ".join(json.load(open("server-rollout.json")).get("retired",[])))')
image=$(docker inspect -f '{{.Config.Image}}' "$active")
dropped=0
cleanup() { rc=$?; rm -f deploy.lock; exit $rc; }
trap cleanup EXIT
restore_backup() {
  echo "FAILED after the drop: restoring $dump" >&2
  pgadmin -c "DROP DATABASE IF EXISTS paperboat WITH (FORCE)" -c "CREATE DATABASE paperboat OWNER postgres"
  docker exec -i paperboat-postgres-1 pg_restore -U postgres -d paperboat --no-owner < "$dump" || true
  docker start "$active" >/dev/null
  echo "RESTORED-FROM-BACKUP $dump" >&2
}
fail() { echo "ERROR: $*" >&2; if [ "$dropped" = 1 ]; then restore_backup; else docker start "$active" >/dev/null 2>&1 || true; fi; exit 1; }
echo "1/8 stop servers (quiesce)"; for c in $retired "$active"; do docker stop --time 30 "$c" >/dev/null || true; done
echo "2/8 fresh backup"; ./backup-database.sh || fail "backup failed"
dump=$(ls -t backups/database-*.dump | head -1); echo "backup: $dump ($(stat -c %s "$dump") bytes)"
echo "3/8 verify the backup restores"; ./final-recheck/verify-backup.sh "$dump" > /tmp/pb-wipe-verify.log 2>&1; rc=$?; tail -n 4 /tmp/pb-wipe-verify.log; [ $rc = 0 ] || fail "backup not verified"
echo "4/8 export the config rows from the verified backup"; out=$(mktemp -d /tmp/pb-wipe.XXXXXX)
./final-recheck/export-config.sh "$dump" "$out" > /tmp/pb-wipe-export.log 2>&1; rc=$?; tail -n 2 /tmp/pb-wipe-export.log; [ $rc = 0 ] || fail "export failed"
echo "5/8 drop and recreate paperboat"; dropped=1
pgadmin -c "DROP DATABASE paperboat WITH (FORCE)" -c "CREATE DATABASE paperboat OWNER postgres" || fail "drop failed"
echo "6/8 migrate with $image"; PAPERBOAT_SERVER_IMAGE="$image" docker compose run --rm migrate || fail "migrate failed"
echo "7/8 import the config rows"; ./final-recheck/import-config.sh "$out" > /tmp/pb-wipe-import.log 2>&1; rc=$?; tail -n 16 /tmp/pb-wipe-import.log; [ $rc = 0 ] || fail "import failed"
echo "8/8 start $active"; docker start "$active" >/dev/null || fail "server did not start"
ok=0; for i in $(seq 1 60); do curl -fsS -o /dev/null "https://$PAPERBOAT_API_HOST/readyz" && ok=1 && break; sleep 2; done
[ "$ok" = 1 ] || fail "server not ready"
curl -fsS "https://$PAPERBOAT_API_HOST/v1/server-version"; echo
rm -rf "$out"
echo "users=$(pg -At -c 'select count(*) from paperboat.users') nodes=$(pg -At -c 'select count(*) from paperboat.control_tunnel_nodes') machines=$(pg -At -c 'select count(*) from paperboat.user_machines')"
echo "WIPE-AND-REBUILD-DONE (backup kept: $dump)"
