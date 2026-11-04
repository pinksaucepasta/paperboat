#!/usr/bin/env bash
# CI end-to-end (R-02): the real server services (server/internal/httpapi TestE2EServe, identity provider
# faked) on PostgreSQL, two systemd containers each running a real `pb` that pairs, then the stages in
# E2E_STAGES (pair exec send preview). Any failing step fails the run.
#   PAPERBOAT_TEST_DATABASE_DSN  isolated *_test database (CI service container)
set -euo pipefail
cd "$(dirname "$0")/../.."
root=$PWD work=$(mktemp -d) stages=${E2E_STAGES:-pair}
host=e2e-server.example.test port=8443
gateway=$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}')
diagnose() { # on failure: what the server answered and what the daemons said
  echo "--- server last requests"; { grep -E "request method|panic" "$work/server.log" | tail -60 | cut -c1-300; } || true
  for c in e2e-a e2e-b; do echo "--- $c daemon"; docker exec -u pbt -e XDG_RUNTIME_DIR=/run/user/1000 $c journalctl --user --no-pager -n 25 2>&1 | cut -c1-300 || true; done
}
trap 'rc=$?; [ $rc -eq 0 ] || diagnose; touch "$work/stop"; sleep 3; docker rm -f e2e-a e2e-b >/dev/null 2>&1 || true; kill $(jobs -p) 2>/dev/null || true' EXIT
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
(cd server && PAPERBOAT_E2E_DIR=$work PAPERBOAT_E2E_LISTEN=0.0.0.0:$port PAPERBOAT_E2E_URL=https://$host:$port \
  PAPERBOAT_E2E_TLS_CERT=$work/tls.pem PAPERBOAT_E2E_TLS_KEY=$work/tls.key \
  go test -v -count=1 -timeout 30m -run "^TestE2EServe\$" ./internal/app >"$work/server.log" 2>&1) &
for _ in $(seq 120); do [ -s "$work/tokens.json" ] && break; sleep 1; done
[ -s "$work/tokens.json" ] || { tail -30 "$work/server.log"; echo "server did not start" >&2; exit 1; }

step "machines"
docker build -q -t e2e-machine -f tools/device-smoke/ubuntu24-systemd.Dockerfile tools/device-smoke >/dev/null
for name in a b; do
  c=e2e-$name
  docker run -d --name $c --privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock \
    --add-host "$host:$gateway" e2e-machine >/dev/null
  sleep 6
  docker cp "$work/ca.pem" $c:/usr/local/share/ca-certificates/e2e-ca.crt
  docker exec $c update-ca-certificates >/dev/null
  docker exec $c bash -c 'loginctl enable-linger pbt; systemctl start user@1000.service'
  docker cp "$work/pb" $c:/home/pbt/pb
  docker exec $c bash -c 'chown pbt:pbt /home/pbt/pb; chmod 755 /home/pbt/pb'
  X="docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 -w /home/pbt $c"
  $X ./pb install >/dev/null
  { if [ -n "${E2E_BREAK:-}" ]; then printf bad-token; else
      python3 -c 'import json,sys;sys.stdout.write(json.load(open(sys.argv[1]))["e2e-"+sys.argv[2]])' "$work/tokens.json" $name; fi; } |
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
if has send; then
  step "send"
  docker exec -u pbt e2e-a bash -c 'echo e2e-file >/home/pbt/e2e.txt'
  pbx e2e-a send /home/pbt/e2e.txt --to e2e-b
  inbox=$(pbx e2e-b inbox path)
  for _ in $(seq 20); do docker exec -u pbt e2e-b grep -rq e2e-file "$inbox" && break; sleep 1; done
  docker exec -u pbt e2e-b grep -rq e2e-file "$inbox"
  echo "stage send: PASS"
fi
if has preview; then
  step "preview"
  echo "preview stage not implemented" >&2; exit 1
fi
