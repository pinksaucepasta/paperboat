#!/usr/bin/env bash
# CI end-to-end, 3 server instances (e2e.yml multi): the real server services (server/internal/app TestE2EServe,
# identity provider faked) on PostgreSQL, systemd containers each running a real `pb` that pairs, then the stages in
# E2E_STAGES (pair exec multi; multi = 3 server instances behind TestE2EProxy, one SIGKILLed). Any failing step fails
# the run. Every other core path is TestE2ECore (e2e.yml core).
#   PAPERBOAT_TEST_DATABASE_DSN  isolated *_test database (CI service container)
set -euo pipefail
cd "$(dirname "$0")/../.."
root=$PWD work=$(mktemp -d) stages=${E2E_STAGES:-pair} pids=()
host=e2e-server.example.test port=8443
gateway=$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}')
diagnose() { # on failure: what the server answered and what the daemons said
  echo "--- server last requests"; { grep -hE "request method|panic" "$work"/proxy.log "$work/server.log" | tail -60 | cut -c1-300; } || true
  echo "--- leader log"; grep -H "leader" "$work"/server?.log 2>&1 | cut -c1-260 | tail -30 || true
  echo "--- advisory locks"; psql "$PAPERBOAT_TEST_DATABASE_DSN" -c "select l.pid,l.granted,a.state,a.backend_start,now() from pg_locks l left join pg_stat_activity a using(pid) where l.locktype='advisory'" 2>&1 || true
  echo "--- machines"; psql "$PAPERBOAT_TEST_DATABASE_DSN" -c "select alias,state,online,seat_state,configured_capabilities,observed_capabilities,last_seen_at,now() from paperboat.user_machines" 2>&1 || true
  for c in e2e-a e2e-b e2e-c; do echo "--- $c daemon"; docker exec -u pbt -e XDG_RUNTIME_DIR=/run/user/1000 $c journalctl --user --no-pager -n 25 2>&1 | cut -c1-300 || true; done
}
trap 'rc=$?; [ $rc -eq 0 ] || diagnose; touch "$work/stop"; sleep 3; docker rm -f e2e-a e2e-b e2e-c >/dev/null 2>&1 || true; kill $(jobs -p) 2>/dev/null || true' EXIT
step() { printf '\n== %s\n' "$*"; }
has() { [[ " $stages " == *" $1 "* ]]; }

step "certificates"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=e2e-ca" -keyout "$work/ca.key" -out "$work/ca.pem" 2>/dev/null
openssl req -newkey rsa:2048 -nodes -subj "/CN=$host" -keyout "$work/tls.key" -out "$work/tls.csr" 2>/dev/null
printf 'subjectAltName=DNS:%s\n' "$host" >"$work/san"
openssl x509 -req -in "$work/tls.csr" -CA "$work/ca.pem" -CAkey "$work/ca.key" -CAcreateserial -days 1 -extfile "$work/san" -out "$work/tls.pem" 2>/dev/null

step "build pb"
v=dev-e2e; p=github.com/pinksaucepasta/paperboat/internal/buildinfo
(cd cli && CGO_ENABLED=0 go build -trimpath -ldflags "-X $p.Version=$v -X $p.Commit=$(git rev-parse HEAD) -X $p.ProtocolVersion=1 -X $p.DefaultServerURL=https://$host:$port -X $p.DefaultReleaseURL=https://$host:$port" -o "$work/pb" ./cmd/pb)

step "server"
(cd server && go test -c -o "$work/server.test" ./internal/app)
instance() { # n [extra env]: one server process; instance 1 seeds the account and writes tokens.json
  local n=$1; shift
  # exec: the background pid is the server process itself, so the SIGKILL below reaches it
  (cd server/internal/app && exec env PAPERBOAT_E2E_DIR=$work PAPERBOAT_E2E_URL=https://$host:$port "$@" \
    "$work/server.test" -test.v -test.timeout 30m -test.run '^TestE2EServe$' >"$work/server$n.log" 2>&1) &
  pids[$n]=$!
}
if has multi; then
  key=e2e:$(head -c32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=')
  multi=(PAPERBOAT_E2E_MINT_KEY=$key PAPERBOAT_E2E_INSTANCES=3)
  instance 1 "${multi[@]}" PAPERBOAT_E2E_INTERNAL=127.0.0.1:18081
else
  instance 1 PAPERBOAT_E2E_LISTEN=0.0.0.0:$port PAPERBOAT_E2E_TLS_CERT=$work/tls.pem PAPERBOAT_E2E_TLS_KEY=$work/tls.key
fi
ln -s server1.log "$work/server.log"
for _ in $(seq 120); do [ -s "$work/tokens.json" ] && break; sleep 1; done
[ -s "$work/tokens.json" ] || { tail -30 "$work/server.log"; echo "server did not start" >&2; exit 1; }
if has multi; then
  for n in 2 3; do instance $n "${multi[@]}" PAPERBOAT_E2E_FOLLOWER=1 PAPERBOAT_E2E_INTERNAL=127.0.0.1:1808$n; done
  (cd server/internal/app && env PAPERBOAT_E2E_DIR=$work PAPERBOAT_E2E_BACKENDS=127.0.0.1:18081,127.0.0.1:18082,127.0.0.1:18083 \
    PAPERBOAT_E2E_LISTEN=0.0.0.0:$port PAPERBOAT_E2E_TLS_CERT=$work/tls.pem PAPERBOAT_E2E_TLS_KEY=$work/tls.key \
    "$work/server.test" -test.v -test.run '^TestE2EProxy$' >"$work/proxy.log" 2>&1) &
fi

step "machines"
docker build -q -t e2e-machine -f tools/e2e/machine.Dockerfile tools/e2e >/dev/null # the image TestE2ECore builds too
machines=(a b); has multi && machines+=(c)
for name in "${machines[@]}"; do
  c=e2e-$name
  docker run -d --name $c --privileged --dns-option single-request --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock \
    --add-host "$host:$gateway" e2e-machine >/dev/null
  sleep 6
  docker cp "$work/ca.pem" $c:/usr/local/share/ca-certificates/e2e-ca.crt
  docker exec $c update-ca-certificates >/dev/null
  docker exec $c bash -c 'loginctl enable-linger pbt; systemctl start user@1000.service'
  docker cp "$work/pb" $c:/home/pbt/pb
  docker exec $c bash -c 'chown pbt:pbt /home/pbt/pb; chmod 755 /home/pbt/pb'
  X="docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 -w /home/pbt $c"
  $X ./pb install >/dev/null
  python3 -c 'import json,sys;sys.stdout.write(json.load(open(sys.argv[1]))["e2e-"+sys.argv[2]])' "$work/tokens.json" $name |
    ${X/docker exec/docker exec -i} bash -c 'umask 077; cat >token'
  for try in 1 2 3 4 5 6; do
    out=$($X .local/bin/pb pair --enrollment-token-file /home/pbt/token --name e2e-$name --json || true)
    grep -q '"ok": *true' <<<"$out" && break
    sleep 10
  done
  grep -q '"ok": *true' <<<"$out" || { echo "pair e2e-$name failed: $out" >&2; exit 1; }
  $X rm -f token
  echo "paired e2e-$name"
done
has pair && echo "stage pair: PASS"

pbx() { docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 -w /home/pbt "$1" .local/bin/pb "${@:2}"; }
if has exec; then
  step "exec"
  [ "$(pbx e2e-a exec e2e-b -- echo e2e-ok)" = e2e-ok ]
  echo "stage exec: PASS"
fi
if has multi; then
  step "multi"
  bound=${E2E_RECONNECT_BOUND:-30}
  : >"$work/exec.log"
  ( while [ ! -e "$work/execstop" ]; do
      t=$(date +%s); r=ok
      [ "$(timeout 25 docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 -w /home/pbt e2e-a .local/bin/pb exec e2e-b -- echo e2e-ok 2>/dev/null)" = e2e-ok ] || r=fail
      echo "$t $(date +%s) $r" >>"$work/exec.log"; sleep 1
    done ) &
  loop=$!
  sleep 20
  victim=
  for n in 1 2 3; do grep -q "worker leader acquired" "$work/server$n.log" && victim=$n; done
  [ -n "$victim" ] || { echo "no instance holds worker leadership" >&2; exit 1; }
  [ "$(grep -l "worker leader acquired" "$work"/server?.log | wc -l)" = 1 ] || { echo "more than one leader" >&2; exit 1; }
  kill -9 "${pids[$victim]}"; killed=$(date +%s); echo "killed leader instance $victim at $killed"
  wait "${pids[$victim]}" || [ $? = 137 ] || { echo "instance $victim did not die of SIGKILL" >&2; exit 1; } # reaps it
  sleep 3
  code=$( (pbx e2e-a machine revoke e2e-c 2>&1 || true) | grep -o -- '--confirm [A-Za-z0-9]\{6\}' | head -1 | cut -d' ' -f2)
  [ -n "$code" ] || { echo "no revoke confirmation code" >&2; exit 1; }
  pbx e2e-a machine revoke e2e-c --confirm "$code"
  revoked=$(psql -At "$PAPERBOAT_TEST_DATABASE_DSN" -c "select count(*) from paperboat.user_machines where alias='e2e-c' and (revoked_at is not null or state in ('revoked','disconnected'))")
  [ "$revoked" = 1 ] || { echo "revoke did not take effect" >&2; exit 1; }
  ! timeout 25 docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 -w /home/pbt e2e-a .local/bin/pb exec e2e-c -- true >/dev/null 2>&1 || { echo "exec reached the revoked machine" >&2; exit 1; }
  echo "revoke took effect"
  sleep 40; touch "$work/execstop"; wait $loop
  python3 tools/e2e/multi_check.py "$work" "$bound" "$killed" "$victim"
  echo "stage multi: PASS"
fi
