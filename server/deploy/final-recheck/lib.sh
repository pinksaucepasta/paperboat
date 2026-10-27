#!/bin/sh
# Shared by the final re-check wipe scripts. Runs on the managed VPS from /opt/paperboat (compose project dir).
# Never prints secrets: server.env and .env are only passed to docker, never echoed.
set -eu
cd /opt/paperboat
. ./.env
pg() { docker exec -i paperboat-postgres-1 psql -U postgres -d paperboat -v ON_ERROR_STOP=1 "$@"; }
pgadmin() { docker exec -i paperboat-postgres-1 psql -U postgres -d postgres -v ON_ERROR_STOP=1 "$@"; }

# Tables an operator or the deployment provisions (not user data): a wipe keeps them by restoring them from the
# verified backup, so the managed edges (nodes, installations, credentials hashes, usage keys, certificates),
# the operator account, billing products, update policy and WorkOS cursor survive. Parents before children.
CONFIG_TABLES="operator_accounts operator_bootstrap billing_products billing_operator_policy client_update_policy \
control_tunnel_nodes selfhost_installations control_usage_verification_keys relay_subject_sets \
edge_dns_provider_budgets workos_event_cursor"

# Tables whose row counts must match exactly between the live database and its restored backup.
KEY_TABLES="users teams billing_workspaces $CONFIG_TABLES"

scratch_start() { # prints the container name
  name=pb-scratch-$$
  docker run -d --name "$name" -e POSTGRES_PASSWORD=scratch -e POSTGRES_DB=paperboat --tmpfs /var/lib/postgresql/data \
    "$PAPERBOAT_POSTGRES_IMAGE" >/dev/null
  for i in $(seq 1 60); do docker exec "$name" pg_isready -U postgres -d paperboat >/dev/null 2>&1 && break; sleep 1; done
  sleep 2
  docker exec "$name" pg_isready -U postgres -d paperboat >/dev/null
  echo "$name"
}

# Platform wildcard certificates are kept too (re-issuing them would spend Let's Encrypt rate limit).
# <table>|<row filter> (filter is a WHERE body evaluated in the source database), parents first.
config_sets() {
  for t in $CONFIG_TABLES; do echo "$t|true"; done
  echo "tunnel_certificate_records|certificate_reference IN (SELECT certificate_reference FROM paperboat.tunnel_platform_certificate_targets)"
  echo "tunnel_platform_certificate_targets|true"
}
# insertable columns of a table in database $2 of container $1
columns() {
  docker exec "$1" psql -U postgres -d "$2" -At -c "select string_agg(quote_ident(column_name), ',' order by ordinal_position) from information_schema.columns where table_schema='paperboat' and table_name='$3' and is_generated='NEVER' and identity_generation is distinct from 'ALWAYS'"
}
# export <container> <srcdb> <outdir> <sets-function>: one .copy file per table (+ .cols)
export_sets() {
  mkdir -p "$3"; chmod 700 "$3"
  $4 | while IFS='|' read -r table filter; do
    cols=$(columns "$1" "$2" "$table")
    printf '%s' "$cols" > "$3/$table.cols"
    docker exec "$1" psql -U postgres -d "$2" -v ON_ERROR_STOP=1 -c "\\copy (SELECT $cols FROM paperboat.$table WHERE $filter) TO STDOUT" > "$3/$table.copy"
    echo "exported $table $(wc -l < "$3/$table.copy") rows"
  done
}
# import <container> <dstdb> <dir> <sets-function> <replica|origin>
import_sets() {
  $4 | while IFS='|' read -r table filter; do
    [ -s "$3/$table.cols" ] || continue
    # migrations seed some config tables (client_update_policy's singleton): the exported rows replace them
    { printf '%s\n' "SET session_replication_role = $5;"; [ "$5" != replica ] || printf '%s\n' "DELETE FROM paperboat.$table;"; printf '%s\n' "\\copy paperboat.$table ($(cat "$3/$table.cols")) FROM STDIN"; cat "$3/$table.copy"; } \
      | docker exec -i "$1" psql -U postgres -d "$2" -v ON_ERROR_STOP=1 -q
    echo "imported $table $(wc -l < "$3/$table.copy") rows"
  done
}
