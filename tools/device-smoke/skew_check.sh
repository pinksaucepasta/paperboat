#!/usr/bin/env bash
# Skew check (release gate): the previous release's pb, edge and relay against a server built from HEAD.
# TestE2ECore in skew mode (server/internal/app, PAPERBOAT_E2E_PREVIOUS): machines on the previous pb pair with
# the HEAD server through the previous edge and relay; every core (pair, exec, ssh, send), edge (tunnel, preview)
# and team path runs; then the edge, relay and each machine's pb move to HEAD in place and everything runs again.
# Runs on our own host (docker + PostgreSQL in ~/.pb-test-pg-dsn), never a GitHub runner; builds happen there.
#   tools/device-smoke/skew_check.sh <previous-release-tag> [host]     (default host: ubuntu@anvit)
# SKEW_CHECK_PUBLISH=1 records the result as the commit status paperboat/skew-check on HEAD, which release.yml
# requires before it publishes a release of that commit: run it on the exact release commit (the tag).
# pb is the released asset from GitHub Releases; edge, relay and the self-host harness are built from the tag.
set -euo pipefail
prev=${1:?usage: skew_check.sh <previous-release-tag> [host]} host=${2:-ubuntu@anvit}
root=$(git rev-parse --show-toplevel)
cd "$root"
git rev-parse -q --verify "refs/tags/$prev" >/dev/null || git fetch -q origin "refs/tags/$prev:refs/tags/$prev"
arch=$(ssh -o BatchMode=yes "$host" uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
mkdir -p "$stage/head" "$stage/previous-src" "$stage/previous"
git archive HEAD -- . ':!console' ':!marketing' ':!admin' | tar x -C "$stage/head"
git archive "$prev" -- . ':!console' ':!marketing' ':!admin' | tar x -C "$stage/previous-src"
gh release download "$prev" --pattern "pb-linux-$arch" --dir "$stage/previous"
mv "$stage/previous/pb-linux-$arch" "$stage/previous/pb"
chmod 755 "$stage/previous/pb"
lane=$(printf %s "${root##*/}" | tr -c 'A-Za-z0-9._-' _)
dest="pb-skew/$lane"
echo "skew-check: $prev ($arch) vs HEAD $(git rev-parse --short HEAD) on $host" >&2
ssh -o BatchMode=yes "$host" "mkdir -p '$dest'"
rsync -az --delete -e 'ssh -o BatchMode=yes' "$stage"/ "$host:$dest/"
sha=$(git rev-parse HEAD)
set +e
ssh -o BatchMode=yes -o ServerAliveInterval=30 "$host" \
  "GITHUB_SHA=$(git rev-parse HEAD) flock -w 7200 \$HOME/pb-remote-test/host.lock bash -s -- $dest" <<'REMOTE'
set -euo pipefail
umask 022
dest="$HOME/$1" lane=${1##*/}
export PATH=$HOME/.local/go1.27.4/bin:$HOME/go-sdk-1.27.4/bin:$PATH GOTOOLCHAIN=local CGO_ENABLED=0
# The previous release's edge, relay and self-host harness, built from its own source.
cd "$dest/previous-src/edge" && go build -trimpath -o "$dest/previous/paperboat-edge" ./cmd/paperboat-edge
cd "$dest/previous-src/relay" && go build -trimpath -o "$dest/previous/paperboat-relay" ./cmd/paperboat-relay
cd "$dest/previous-src/relay" && go test -c -o "$dest/previous/selfhost-node.test" ./selfhost
# Host prep e2e.yml does on its runner: the server's test name and low ports for the edge.
grep -q ' e2e-server.example.test$' /etc/hosts || echo "127.0.0.1 e2e-server.example.test" | sudo -n tee -a /etc/hosts >/dev/null
sudo -n sysctl -q net.ipv4.ip_unprivileged_port_start=80
# A fresh database for this run, like tools/remote-test.sh.
base=$(cat "$HOME/.pb-test-pg-dsn") db="lane_skew_$(tr -c 'A-Za-z0-9' _ <<<"$lane")test"
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
