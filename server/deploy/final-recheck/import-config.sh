#!/bin/sh
# usage: import-config.sh <exportdir> [container] [database]    (default: prod postgres container, database paperboat)
# Into a freshly migrated database: config rows (triggers off, they are consistent by construction), then the
# fixture rows with triggers on (as a real sign-in and team creation would insert them), then the paid
# entitlement as test data: the team's billing period reset to now-1h .. now+90d. Verifies and prints counts.
. "$(dirname "$0")/lib.sh"
dir=$1; c=${2:-paperboat-postgres-1}; db=${3:-paperboat}
import_sets "$c" "$db" "$dir/config" config_sets replica
import_sets "$c" "$db" "$dir/fixture" fixture_sets origin
docker exec "$c" psql -U postgres -d "$db" -v ON_ERROR_STOP=1 -q -c "UPDATE paperboat.billing_workspaces SET state='active', period_start=now()-interval '1 hour', period_end=now()+interval '90 days', cancel_at_period_end=false WHERE team_id='$SMOKE_TEAM'"
# no imported row may point at a row that does not exist (foreign keys were off for the config tables)
docker exec "$c" psql -U postgres -d "$db" -At -v ON_ERROR_STOP=1 -c "
DO \$\$ DECLARE r record; n bigint; BEGIN
 FOR r IN SELECT c.conrelid::regclass AS child, c.confrelid::regclass AS parent, a.attname AS ccol, pa.attname AS pcol
  FROM pg_constraint c JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=c.conkey[1] JOIN pg_attribute pa ON pa.attrelid=c.confrelid AND pa.attnum=c.confkey[1]
  WHERE c.contype='f' AND array_length(c.conkey,1)=1 AND c.connamespace='paperboat'::regnamespace LOOP
  EXECUTE format('SELECT count(*) FROM %s c WHERE c.%I IS NOT NULL AND NOT EXISTS (SELECT 1 FROM %s p WHERE p.%I=c.%I)', r.child, r.ccol, r.parent, r.pcol, r.ccol) INTO n;
  IF n > 0 THEN RAISE EXCEPTION 'orphan rows: % rows of % point at a missing % row', n, r.child, r.parent; END IF;
 END LOOP; END \$\$;"
for t in users user_identities teams team_members billing_workspaces control_tunnel_nodes selfhost_installations operator_accounts billing_products client_update_policy tunnel_certificate_records tunnel_platform_certificate_targets; do
  echo "$t $(docker exec "$c" psql -U postgres -d "$db" -At -c "select count(*) from paperboat.$t")"
done
paid=$(docker exec "$c" psql -U postgres -d "$db" -At -c "select paperboat.team_paid_entitled('$SMOKE_TEAM')")
echo "team_paid_entitled($SMOKE_TEAM) = $paid"
[ "$paid" = t ] && echo "IMPORTED"
