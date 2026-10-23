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
# DOCKER="sudo docker" where the user is not in the docker group.
DOCKER=${DOCKER:-docker}
SERVER=${1:?paperboat-server binary}; TESTS=${2:?app.load.test}; N=${3:?instances}; RUN=${4:-TestLoad}
WORK=$(mktemp -d); PG=paperboat-load-pg; PROXY=paperboat-load-proxy; PGPORT=55432; PROXYPORT=58080
PASS=$(head -c 18 /dev/urandom | base64 | tr -dc A-Za-z0-9)
pids=()
cleanup() {
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  [ -n "${stats:-}" ] && kill "$stats" 2>/dev/null || true
  [ -n "${watcher:-}" ] && kill "$watcher" 2>/dev/null || true
  $DOCKER rm -f "$PG" "$PROXY" >/dev/null 2>&1 || true
  $DOCKER volume rm paperboat-load-pgdata >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT
$DOCKER rm -f "$PG" "$PROXY" >/dev/null 2>&1 || true
$DOCKER run -d --name "$PG" -p 127.0.0.1:$PGPORT:5432 -v paperboat-load-pgdata:/var/lib/postgresql/data \
  -e POSTGRES_PASSWORD="$PASS" -e POSTGRES_DB=paperboat_load_test postgres:17 \
  -c max_connections=400 -c max_locks_per_transaction=2048 -c shared_buffers=${PAPERBOAT_LOAD_PG_SHARED_BUFFERS:-256MB} >/dev/null
# The image's first start runs initdb on a temporary server (pg_isready already answers) and then restarts it;
# connect only after the init process completed and the final server accepts connections.
for _ in $(seq 1 120); do
  $DOCKER logs "$PG" 2>&1 | grep -q "PostgreSQL init process complete" \
    && $DOCKER exec "$PG" pg_isready -U postgres -d paperboat_load_test >/dev/null 2>&1 && break
  sleep 1
done
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
  PAPERBOAT_RELEASE_BASE_URL=https://api.example.test PAPERBOAT_CLI_VERIFICATION_URL=http://console.example.test/cli \
  PAPERBOAT_MACHINES_URL=http://console.example.test/machines PAPERBOAT_LOCAL_ACCESS_BASE_DOMAIN=local.example.test \
  PAPERBOAT_RUNTIME_BASE_DOMAIN=runtime.example.test PAPERBOAT_PREVIEW_BASE_DOMAIN=preview.example.test \
  PAPERBOAT_TUNNEL_BASE_DOMAIN=tunnel.example.test \
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
$DOCKER run -d --name "$PROXY" --network host -v "$WORK/Caddyfile:/etc/caddy/Caddyfile:ro" caddy:2 >/dev/null
for i in $(seq 1 "$N"); do
  for t in $(seq 1 90); do
    curl -fsS "http://127.0.0.1:$((18080 + i))/healthz" >/dev/null 2>&1 && break
    if ! kill -0 "$(cat "$WORK/server-$i.pid")" 2>/dev/null || [ "$t" = 90 ]; then
      echo "server-$i did not start:"; tail -5 "$WORK/server-$i.log"; exit 1
    fi
    sleep 1
  done
done
until curl -fsS "http://127.0.0.1:$PROXYPORT/healthz" >/dev/null 2>&1; do sleep 1; done
echo "instances=$N up behind :$PROXYPORT"

# Postgres CPU every 5 s for the whole run.
( while sleep 5; do $DOCKER stats --no-stream --format "{{.CPUPerc}}" "$PG" | sed "s/^/pg_cpu $(date +%T) /"; done ) &
stats=$!

export PAPERBOAT_PERF_DATABASE_DSN=$DSN PAPERBOAT_LOAD_TARGET_URL=http://127.0.0.1:$PROXYPORT
# Worker leader (F21-04): paperboat.worker_leader holds the leading instance as "<host>/<pid>"; every instance reports
# paperboat_server_worker_mode (leader|standby|drained) on its loopback /metrics.
leader_row() { $DOCKER exec "$PG" psql -U postgres -d paperboat_load_test -Atc \
  "SELECT instance||' '||to_char(heartbeat_at,'HH24:MI:SS') FROM paperboat.worker_leader" 2>/dev/null; }
mode_of() { curl -fsS --max-time 2 "http://127.0.0.1:$((18080 + $1))/metrics" 2>/dev/null | python3 -c '
import json, sys
def walk(value, path=""):
    if isinstance(value, dict):
        for k, v in value.items(): yield from walk(v, path + "/" + k)
    elif "worker_mode" in path and value in (1, 1.0, True): yield path
for path in walk(json.load(sys.stdin)):
    for mode in ("leader", "standby", "drained"):
        if mode in path: print(mode)' 2>/dev/null | head -1; }
for _ in $(seq 1 60); do [ -n "$(leader_row)" ] && break; sleep 1; done
read -r leader_instance _ <<<"$(leader_row)"
leader_pid=${leader_instance##*/}
victim=""
for i in $(seq 1 "$N"); do [ "$(cat "$WORK/server-$i.pid")" = "$leader_pid" ] && victim=$i; done
if [ -z "$victim" ]; then echo "worker leader $leader_instance is not one of this run's instances"; exit 1; fi
echo "leader before: $leader_instance (server-$victim); modes: $(for i in $(seq 1 "$N"); do printf 'server-%s=%s ' "$i" "$(mode_of "$i")"; done)"
# The failover test kills the leader (worst case); its process id is in a file so nothing matches by name.
export PAPERBOAT_LOAD_KILL_COMMAND="kill -9 $leader_pid"
# Every 2 s: the lease row and how many live instances report mode=leader (exactly once means never 2).
( while sleep 2; do
    leaders=0; modes=""
    for i in $(seq 1 "$N"); do kill -0 "$(cat "$WORK/server-$i.pid")" 2>/dev/null || { modes="$modes server-$i=dead"; continue; }
      m=$(mode_of "$i"); modes="$modes server-$i=${m:-?}"; [ "$m" = leader ] && leaders=$((leaders+1)); done
    echo "leader $(date +%T) row=$(leader_row | tr ' ' '@') leaders=$leaders$modes"
  done ) > "$WORK/leader.log" &
watcher=$!
"$TESTS" -test.run "$RUN" -test.v -test.timeout 90m 2>&1 | grep -vE '^\s*$'
kill "$stats" "$watcher" 2>/dev/null || true
echo "leader samples: $(wc -l < "$WORK/leader.log"); with 2+ leaders: $(grep -c 'leaders=[2-9]' "$WORK/leader.log"); with 0 leaders: $(grep -c 'leaders=0' "$WORK/leader.log")"
awk '{print $2, $3, $4}' "$WORK/leader.log" | uniq -f1 | head -20
cp "$WORK/leader.log" "${PAPERBOAT_LOAD_LEADER_LOG:-leader.log}"
