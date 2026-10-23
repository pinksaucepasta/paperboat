#!/bin/bash
# Horizontal-scaling run for the PLAN 3b scale matrix, on the dedicated staging VM only (never production): one
# throwaway PostgreSQL 17, N paperboat-server instances on one database, Caddy in front (round robin, retry on a
# dead upstream, active health checks), then the server load tests through the proxy:
#   TestLoadIdlePollMix and TestLoadHeldEventWaiters for machines per vCPU at N instances, and TestLoadFailover,
#   which kills one instance mid-test.
# Prints Postgres container CPU next to each step so the run shows where the database becomes the limit.
#
#   tools/loadtest/run-multi-instance.sh <paperboat-server binary> <app.load.test> <instances> [test regex]
#
# Build both from a clean tree:
#   (cd server && CGO_ENABLED=0 go build -o paperboat-server ./cmd/paperboat-server \
#      && CGO_ENABLED=0 go test -c -tags load -o app.load.test ./internal/app/)
set -euo pipefail
SERVER=${1:?paperboat-server binary}; TESTS=${2:?app.load.test}; N=${3:?instances}; RUN=${4:-TestLoad}
WORK=$(mktemp -d); PG=paperboat-load-pg; PROXY=paperboat-load-proxy; PGPORT=55432; PROXYPORT=58080
PASS=$(head -c 18 /dev/urandom | base64 | tr -dc A-Za-z0-9)
pids=()
cleanup() {
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  [ -n "${stats:-}" ] && kill "$stats" 2>/dev/null || true
  docker rm -f "$PG" "$PROXY" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT
docker rm -f "$PG" "$PROXY" >/dev/null 2>&1 || true
docker run -d --name "$PG" -p 127.0.0.1:$PGPORT:5432 --tmpfs /var/lib/postgresql/data:rw,size=6g \
  -e POSTGRES_PASSWORD="$PASS" -e POSTGRES_DB=paperboat_load_test postgres:17 \
  -c max_connections=400 -c max_locks_per_transaction=2048 -c shared_buffers=512MB >/dev/null
until docker exec "$PG" pg_isready -U postgres -d paperboat_load_test >/dev/null 2>&1; do sleep 1; done
DSN="postgres://postgres:$PASS@127.0.0.1:$PGPORT/paperboat_load_test?sslmode=disable"

# Migrate and seed once through the test binary (it refuses non-*_test databases), before any instance starts.
PAPERBOAT_PERF_DATABASE_DSN=$DSN "$TESTS" -test.run '^TestLoadSeedOnly$' -test.v >"$WORK/seed.log" 2>&1 || { tail -20 "$WORK/seed.log"; exit 1; }

upstreams=""
for i in $(seq 1 "$N"); do
  port=$((18080 + i))
  # Throwaway values: no WorkOS call is on the load paths; the session is seeded directly.
  PAPERBOAT_ENV=development PAPERBOAT_HTTP_ADDRESS=127.0.0.1:$port PAPERBOAT_DATABASE_DSN=$DSN \
  PAPERBOAT_PUBLIC_BASE_URL=http://api.example.test PAPERBOAT_BROWSER_LOGIN_ORIGIN=http://console.example.test \
  PAPERBOAT_WORKOS_CLIENT_ID=load-test PAPERBOAT_WORKOS_API_KEY=load-test \
    "$SERVER" serve >"$WORK/server-$i.log" 2>&1 &
  pids+=($!); echo "$!" >"$WORK/server-$i.pid"
  upstreams="$upstreams 127.0.0.1:$port"
done
cat >"$WORK/Caddyfile" <<EOF
{
	auto_https off
	admin off
}
:$PROXYPORT {
	reverse_proxy$upstreams {
		lb_policy round_robin
		lb_try_duration 5s
		fail_duration 10s
		health_uri /healthz
		health_interval 2s
	}
}
EOF
docker run -d --name "$PROXY" --network host -v "$WORK/Caddyfile:/etc/caddy/Caddyfile:ro" caddy:2 >/dev/null
for i in $(seq 1 "$N"); do
  until curl -fsS "http://127.0.0.1:$((18080 + i))/healthz" >/dev/null 2>&1; do sleep 1; done
done
until curl -fsS "http://127.0.0.1:$PROXYPORT/healthz" >/dev/null 2>&1; do sleep 1; done
echo "instances=$N up behind :$PROXYPORT"

# Postgres CPU every 5 s for the whole run.
( while sleep 5; do docker stats --no-stream --format "{{.CPUPerc}}" "$PG" | sed "s/^/pg_cpu $(date +%T) /"; done ) &
stats=$!

export PAPERBOAT_PERF_DATABASE_DSN=$DSN PAPERBOAT_LOAD_TARGET_URL=http://127.0.0.1:$PROXYPORT
# The failover test kills the first instance; its process id is in a file so nothing matches by name.
export PAPERBOAT_LOAD_KILL_COMMAND="kill -9 \$(cat $WORK/server-1.pid)"
"$TESTS" -test.run "$RUN" -test.v -test.timeout 90m 2>&1 | grep -vE '^\s*$'
kill "$stats" 2>/dev/null || true
# Background workers must run in one instance only (F21-04): show which instances logged worker activity.
for i in $(seq 1 "$N"); do
  echo "server-$i worker log lines: $(grep -ciE 'worker|leader|reconcil' "$WORK/server-$i.log" || true)"
done
