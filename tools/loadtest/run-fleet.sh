#!/bin/bash
# Simulated machine fleet against the real router and an isolated PostgreSQL 17 with pg_stat_statements, on a staging
# VM (never the production host or database). Per step it prints the server's and PostgreSQL's CPU cores per machine
# (server/internal/app/load_fleet_test.go) and PostgreSQL's top statements by total time and calls.
#
#   tools/loadtest/run-fleet.sh <app.load.test> [machines,...]    default 500,1500,3000
#
# Build the binary from a clean tree: (cd server && CGO_ENABLED=0 go test -c -tags load -o app.load.test ./internal/app/)
# Env: PAPERBOAT_FLEET_SECONDS (measured window, 300), PAPERBOAT_FLEET_ACCOUNTS (300), PAPERBOAT_FLEET_SETTLE_SECONDS (90),
# PAPERBOAT_FLEET_OUT (result directory, default ./fleet-results).
set -euo pipefail
DOCKER=${DOCKER:-docker}
BIN=$(readlink -f "${1:?path to app.load.test}"); STEPS=${2:-500,1500,3000}
NAME=paperboat-fleet-pg; PORT=${PAPERBOAT_FLEET_PG_PORT:-55434}; LISTEN=127.0.0.1:${PAPERBOAT_FLEET_PORT:-18480}
OUT=${PAPERBOAT_FLEET_OUT:-./fleet-results}; mkdir -p "$OUT"
PASS=$(head -c 18 /dev/urandom | base64 | tr -dc A-Za-z0-9)
server_pid=""
cleanup() { [ -z "$server_pid" ] || kill "$server_pid" 2>/dev/null || true; $DOCKER rm -f paperboat-fleet-caddy >/dev/null 2>&1 || true; $DOCKER rm -f "$NAME" >/dev/null 2>&1 || true; $DOCKER volume rm paperboat-fleet-pgdata >/dev/null 2>&1 || true; }
trap cleanup EXIT; cleanup
ulimit -n 65536 || true
$DOCKER run -d --name "$NAME" -p 127.0.0.1:"$PORT":5432 -v paperboat-fleet-pgdata:/var/lib/postgresql/data \
  -e POSTGRES_PASSWORD="$PASS" -e POSTGRES_DB=paperboat_fleet_test postgres:17 \
  -c max_connections=400 -c max_locks_per_transaction=2048 -c shared_buffers=${PAPERBOAT_FLEET_SHARED_BUFFERS:-512MB} \
  -c shared_preload_libraries=pg_stat_statements -c pg_stat_statements.track=all -c pg_stat_statements.max=5000 >/dev/null
for _ in $(seq 1 120); do
  $DOCKER logs "$NAME" 2>&1 | grep -q "PostgreSQL init process complete" \
    && $DOCKER exec "$NAME" pg_isready -U postgres -d paperboat_fleet_test >/dev/null 2>&1 && break
  sleep 1
done
psql() { $DOCKER exec -i "$NAME" psql -U postgres -d paperboat_fleet_test -v ON_ERROR_STOP=1 "$@"; }
psql -qc "CREATE EXTENSION pg_stat_statements" >/dev/null
export PAPERBOAT_PERF_DATABASE_DSN="postgres://postgres:$PASS@127.0.0.1:$PORT/paperboat_fleet_test?sslmode=disable"
export PAPERBOAT_FLEET_LISTEN=$LISTEN PAPERBOAT_FLEET_URL=http://$LISTEN PAPERBOAT_FLEET_PG_CONTAINER=$NAME
"$BIN" -test.run '^TestFleetServe$' -test.v -test.timeout 6h >"$OUT/server.log" 2>&1 &
server_pid=$!
for _ in $(seq 1 600); do curl -fs "http://$LISTEN/healthz" >/dev/null 2>&1 && break; sleep 1; done
curl -fs "http://$LISTEN/healthz" >/dev/null || { echo "server did not become ready" >&2; tail -20 "$OUT/server.log" >&2; exit 1; }
export PAPERBOAT_FLEET_SERVER_PID=$server_pid
# PAPERBOAT_FLEET_PROXY=1: the machines reach the server through Caddy with deploy/Caddyfile's API-host block (the
# compressible-list encoding and stream_close_delay; the edge-control log is only for /v1/edge/* and is omitted), on
# host networking with its internal CA, as the production proxy fronts the server. Its cgroup CPU is reported per step.
if [ -n "${PAPERBOAT_FLEET_PROXY:-}" ]; then
  PROXY_ADDR=127.0.0.1:${PAPERBOAT_FLEET_PROXY_PORT:-18443}
  mkdir -p "$OUT/caddy"
  cat > "$OUT/caddy/Caddyfile" <<CADDY
{
	auto_https disable_redirects
	admin off
}
localhost:${PROXY_ADDR##*:} {
	tls internal
	@compressible expression \`(method('GET') && path('/v1/machines', '/v1/machines/*/terminal-sessions', '/v1/terminal-sessions', '/v1/previews', '/v1/tunnels', '/v1/machine-services')) || (method('POST') && path('/v1/machine-peer-identity/status'))\`
	encode @compressible zstd gzip
	reverse_proxy $LISTEN {
		stream_close_delay 10m
	}
}
CADDY
  $DOCKER rm -f paperboat-fleet-caddy >/dev/null 2>&1 || true
  $DOCKER run -d --name paperboat-fleet-caddy --network host -v "$(cd "$OUT/caddy" && pwd)/Caddyfile":/etc/caddy/Caddyfile:ro caddy:2 >/dev/null
  for _ in $(seq 1 60); do curl -ksf "https://localhost:${PROXY_ADDR##*:}/healthz" >/dev/null 2>&1 && break; sleep 1; done
  curl -ksf "https://localhost:${PROXY_ADDR##*:}/healthz" >/dev/null || { echo "proxy did not answer" >&2; $DOCKER logs paperboat-fleet-caddy | tail -20 >&2; exit 1; }
  export PAPERBOAT_FLEET_URL=https://localhost:${PROXY_ADDR##*:} PAPERBOAT_FLEET_PROXY_CONTAINER=paperboat-fleet-caddy
fi
for step in ${STEPS//,/ }; do
  psql -qc "SELECT pg_stat_statements_reset()" >/dev/null
  PAPERBOAT_FLEET_MACHINES=$step "$BIN" -test.run '^TestFleetRun$' -test.v -test.timeout 2h | tee "$OUT/step-$step.log"
  psql -c "SELECT calls, round(total_exec_time::numeric/1000,1) AS total_s, round(mean_exec_time::numeric,3) AS mean_ms, rows, left(regexp_replace(query, '\s+', ' ', 'g'), 150) AS query
           FROM pg_stat_statements WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database()) ORDER BY total_exec_time DESC LIMIT 25" | tee "$OUT/step-$step-statements.txt"
  psql -c "SELECT schemaname||'.'||relname AS tbl, n_tup_ins, n_tup_upd, n_tup_del, seq_scan, idx_scan FROM pg_stat_user_tables ORDER BY n_tup_ins+n_tup_upd+n_tup_del DESC LIMIT 15" | tee "$OUT/step-$step-tables.txt"
done
