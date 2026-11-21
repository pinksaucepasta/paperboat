#!/usr/bin/env bash
# Skew check (release gate): the previous release's pb, edge, relay and pbh against a server built from HEAD.
# TestE2ECore in skew mode (server/internal/app, PAPERBOAT_E2E_PREVIOUS): machines on the previous pb pair with
# the HEAD server through the previous edge and relay; every core (pair, exec, ssh, send), edge (tunnel, preview)
# and team path runs; then the edge, relay and each machine's pb move to HEAD in place and everything runs again.
# Runs on a Linux host with Docker and PostgreSQL. Without a host argument, use the configured
# skew host or start a disposable Linux CI runner for the duration of the check.
#   tools/device-smoke/skew_check.sh [previous-ref] [host]
#   tools/device-smoke/skew_check.sh --preflight <host>  # only check the specified host
# previous-ref is any git ref (a release tag, or origin/main before the first release exists).
# SKEW_CHECK_PUBLISH=1 records the result as the commit status paperboat/skew-check on HEAD, which release.yml
# requires before it publishes a release of that commit: run it on the exact release commit (the tag).
# pb, edge, relay, pbh and the self-host harness are all built from previous-ref's source on the host.
set -euo pipefail
root=$(git rev-parse --show-toplevel)
cd "$root"
preflight() {
# Fail fast on the host problems that held earlier runs: no docker or database, a clash on the edge's 80/443, a full disk.
ssh -o BatchMode=yes "$1" bash -s <<'PREFLIGHT' || { echo "skew-check: host is not ready for the e2e (see above); nothing was run" >&2; exit 1; }
fail() { echo "skew-check preflight: $*" >&2; exit 1; }
docker info >/dev/null 2>&1 || fail "docker is not running or not usable by $USER"
[ -s "$HOME/.pb-test-pg-dsn" ] || fail "$HOME/.pb-test-pg-dsn is missing: the e2e needs a PostgreSQL DSN there"
base=$(cat "$HOME/.pb-test-pg-dsn")
psql "${base%%lane_test*}postgres?sslmode=disable" -qtAc 'select 1' >/dev/null 2>&1 || fail "PostgreSQL does not answer on the DSN in ~/.pb-test-pg-dsn"
gw=$(docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}') || fail "docker has no bridge network"
busy=$(ss -ltunH | awk -v gw="$gw" '$5 ~ ("^(" gw "|0\\.0\\.0\\.0|\\*|\\[::\\]):(80|443)$") {print $1, $5}')
[ -z "$busy" ] || fail "port 80/443 is taken on the docker bridge $gw (the e2e edge needs both): $busy"
[ "$(df --output=avail -BG / | tail -n1 | tr -dc 0-9)" -ge 20 ] || fail "less than 20 GB free on /: $(df -h --output=avail / | tail -n1)"
PREFLIGHT
}
targets=$root/tools/device-smoke/dev_targets.py
if [ "${1:-}" = --preflight ]; then
  [ "$#" -eq 2 ] || { echo 'skew-check: --preflight requires an SSH host' >&2; exit 2; }
  host=$(python3 "$targets" ssh "$2")
  preflight "$host"
  exit
fi
prev=${1:-$(gh release view --json tagName -q .tagName)} host=${2:-}
[ -n "$host" ] || host=$(python3 "$targets" skew_host)
if [ -n "$host" ]; then host=$(python3 "$targets" ssh "$host"); fi
if [ -z "$host" ]; then
  pool=$root/tools/device-smoke/linuxpool.sh
  n=$("$pool" start 1 2 | tail -n1) && [[ $n =~ ^linuxpool-[0-9]+$ ]] || { echo "skew-check: no linuxpool runner started" >&2; exit 1; }
  host=$n
  trap '"$pool" stop "${host#linuxpool-}" >&2 || true' EXIT
fi
preflight "$host"
git fetch -q origin --tags
prev_sha=$(git rev-parse --verify "$prev^{commit}")
arch=$(ssh -o BatchMode=yes "$host" uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
stage=$(mktemp -d)
trap '[ -z "${pool:-}" ] || "$pool" stop "${host#linuxpool-}" >&2 || true; rm -rf "$stage"' EXIT
mkdir -p "$stage/head" "$stage/previous-src" "$stage/previous"
git archive HEAD -- . ':!console' ':!marketing' ':!admin' | tar x -C "$stage/head"
git archive "$prev_sha" -- . ':!console' ':!marketing' ':!admin' | tar x -C "$stage/previous-src"
worktree=$(printf %s "${root##*/}" | tr -c 'A-Za-z0-9._-' _)
dest="pb-skew/$worktree"
echo "skew-check: $prev ${prev_sha:0:9} ($arch) vs HEAD $(git rev-parse --short HEAD) on $host" >&2
ssh -o BatchMode=yes "$host" "mkdir -p '$dest' \$HOME/pb-remote-test"
rsync -az --delete -e 'ssh -o BatchMode=yes' "$stage"/ "$host:$dest/"
sha=$(git rev-parse HEAD)
set +e
ssh -o BatchMode=yes -o ServerAliveInterval=30 "$host" \
  "GITHUB_SHA=$sha PREV_SHA=$prev_sha flock -w 7200 \$HOME/pb-remote-test/host.lock bash -s -- $dest" <<'REMOTE'
set -euo pipefail
umask 022
# A cache of this run only: no other job on the host can clean it away mid-build.
export GOCACHE=$(mktemp -d "$HOME/pb-skew/gocache.XXXXXX")
trap 'rm -rf "$GOCACHE"' EXIT
dest="$HOME/$1" worktree=${1##*/}
export PATH=$HOME/.local/go1.27.4/bin:$HOME/go-sdk-1.27.4/bin:$PATH GOTOOLCHAIN=local CGO_ENABLED=0
# The previous release's pb, edge, relay and self-host harness, built from its own source.
b=github.com/pinksaucepasta/paperboat/cli/internal/buildinfo
cd "$dest/previous-src/cli" && go build -trimpath -ldflags "-X $b.Version=dev-previous -X $b.Commit=$PREV_SHA -X $b.ProtocolVersion=1" -o "$dest/previous/pb" ./cmd/pb
cd "$dest/previous-src/edge" && go build -trimpath -o "$dest/previous/paperboat-edge" ./cmd/paperboat-edge
cd "$dest/previous-src/relay" && go build -trimpath -o "$dest/previous/paperboat-relay" ./cmd/paperboat-relay
cd "$dest/previous-src/relay" && go test -c -o "$dest/previous/selfhost-node.test" ./selfhost
# pbh: the binary, and its server-facing client code as a test binary the e2e runs against the HEAD server.
cd "$dest/previous-src/helper" && go build -trimpath -o "$dest/previous/pbh" ./cmd/pbh
# A previous release without the probe test gets no binary; the e2e then skips the probe.
if grep -q "func TestPbhServerProbe" "$dest"/previous-src/helper/internal/containers/*_test.go; then
  cd "$dest/previous-src/helper" && go test -c -o "$dest/previous/pbh-probe.test" ./internal/containers
fi
# Host prep e2e.yml does on its runner: the server's test name and low ports for the edge.
grep -q ' e2e-server.example.test$' /etc/hosts || echo "127.0.0.1 e2e-server.example.test" | sudo -n tee -a /etc/hosts >/dev/null
sudo -n sysctl -q net.ipv4.ip_unprivileged_port_start=80
# A fresh database for this run, like tools/remote-test.sh.
base=$(cat "$HOME/.pb-test-pg-dsn") db="lane_skew_$(tr -c 'A-Za-z0-9' _ <<<"$worktree")test"
psql "${base%%lane_test*}postgres?sslmode=disable" -qc "drop database if exists \"$db\" with (force)" -c "create database \"$db\""
cd "$dest/head/server"
PAPERBOAT_E2E_CORE=1 PAPERBOAT_E2E_PREVIOUS="$dest/previous" PAPERBOAT_TEST_DATABASE_DSN="${base/lane_test\?/$db?}" \
  nice go test -count=1 -timeout 60m -v -run '^TestE2ECore$' ./internal/app | tee "$dest/skew.log"
grep -q -- '^--- PASS: TestE2ECore ' "$dest/skew.log"
REMOTE
rc=$?
set -e
if [ "${SKEW_CHECK_PUBLISH:-}" = 1 ]; then
  state=failure
  [ "$rc" = 0 ] && state=success
  gh api "repos/{owner}/{repo}/statuses/$sha" -f state="$state" -f context=paperboat/skew-check \
    -f description="previous release $prev against this commit's server, then updated to it" >/dev/null
fi
exit "$rc"
