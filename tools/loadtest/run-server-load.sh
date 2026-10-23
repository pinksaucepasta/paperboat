#!/bin/bash
# Server load run for the PLAN 3b scale matrix, on a dedicated staging VM (never the production host or database).
# Starts a throwaway PostgreSQL 17 on tmpfs, runs the server load test binary against it (it migrates and seeds the
# perf data set on first use), then removes the database container.
#
#   tools/loadtest/run-server-load.sh <app.load.test> [test regex] [extra test flags]
#
# Build the binary from a clean tree: (cd server && CGO_ENABLED=0 go test -c -tags load -o app.load.test ./internal/app/)
# Step sizes: PAPERBOAT_LOAD_WAITERS=100,500,1000 PAPERBOAT_LOAD_DAEMONS=50,100,250 (defaults in load_postgres_test.go).
set -euo pipefail
# DOCKER="sudo docker" where the user is not in the docker group.
DOCKER=${DOCKER:-docker}
BIN=${1:?path to app.load.test}; RUN=${2:-TestLoad}; shift $(( $# > 2 ? 2 : $# ))
NAME=paperboat-load-pg
PORT=${PAPERBOAT_LOAD_PG_PORT:-55432}
PASS=$(head -c 18 /dev/urandom | base64 | tr -dc A-Za-z0-9)
cleanup() { $DOCKER rm -f "$NAME" >/dev/null 2>&1 || true; $DOCKER volume rm paperboat-load-pgdata >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup
$DOCKER run -d --name "$NAME" -p 127.0.0.1:"$PORT":5432 -v paperboat-load-pgdata:/var/lib/postgresql/data \
  -e POSTGRES_PASSWORD="$PASS" -e POSTGRES_DB=paperboat_load_test postgres:17 \
  -c max_connections=400 -c max_locks_per_transaction=2048 -c shared_buffers=${PAPERBOAT_LOAD_PG_SHARED_BUFFERS:-256MB} \
  -c shared_preload_libraries=auto_explain -c auto_explain.log_min_duration=2s -c auto_explain.log_analyze=on \
  -c auto_explain.log_buffers=on -c auto_explain.log_nested_statements=on >/dev/null
for _ in $(seq 1 60); do
  $DOCKER exec "$NAME" pg_isready -U postgres -d paperboat_load_test >/dev/null 2>&1 && break
  sleep 1
done
export PAPERBOAT_PERF_DATABASE_DSN="postgres://postgres:$PASS@127.0.0.1:$PORT/paperboat_load_test?sslmode=disable"
"$BIN" -test.run "$RUN" -test.v -test.timeout 90m "$@"
# Plans of every statement slower than 2 s during the run (auto_explain), for the owner of the slow query.
$DOCKER logs "$NAME" 2>&1 | grep -A60 "duration: [0-9]*\.[0-9]* ms  plan:" | head -400 > "${PAPERBOAT_LOAD_SLOW_PLANS:-slow-plans.txt}" || true
echo "slow statement plans: $(grep -c "plan:" "${PAPERBOAT_LOAD_SLOW_PLANS:-slow-plans.txt}" || true) (in ${PAPERBOAT_LOAD_SLOW_PLANS:-slow-plans.txt})"
