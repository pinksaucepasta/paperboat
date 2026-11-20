#!/usr/bin/env bash
# Run one module's Go tests for the committed HEAD on a spare VM instead of the shared build host.
#   tools/remote-test.sh <host> <module> [go test args...]
# The host is an SSH destination or an alias in the local development target configuration.
# Use a Linux host with Go, gcc, and passwordless sudo for CLI system fixtures. A host with
# ~/.pb-test-pg-dsn runs PostgreSQL suites in separate databases. Each host runs one job at a time.
# Only committed files are synced, and GITHUB_SHA identifies the source revision.
set -euo pipefail

usage() { sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
[ $# -ge 2 ] || usage
host=$1 module=$2
shift 2
case $module in shared | cli | relay | edge | helper | server) ;; *) echo "remote-test: unknown module '$module'" >&2; usage ;; esac

root=$(git rev-parse --show-toplevel)
cd "$root"
host=$(python3 tools/device-smoke/dev_targets.py ssh "$host")
[ -d "$module" ] || { echo "remote-test: no $module/ in $root" >&2; exit 2; }
if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
  echo "remote-test: note: uncommitted changes are NOT tested (only HEAD $(git rev-parse --short HEAD))" >&2
fi

wanted_pkg=0
for a in "$@"; do case $a in ./* | ...) wanted_pkg=1 ;; esac; done
[ "$wanted_pkg" = 1 ] || set -- "$@" ./...

# One sync tree per worktree, so two worktrees never overwrite each other's sources; this script
# serializes against itself per worktree and host.
worktree=$(printf %s "${root##*/}" | tr -c 'A-Za-z0-9._-' _)
exec 8>"${TMPDIR:-/tmp}/pb-remote-test-$host-$worktree.lock"
flock 8

stage=$(mktemp -d "${TMPDIR:-/tmp}/pb-remote-test.XXXXXX")
trap 'rm -rf "$stage"' EXIT
git archive HEAD -- . ':!console' ':!marketing' | tar x -C "$stage"
sha=$(git rev-parse --short HEAD)

echo "remote-test: syncing $sha to $host:pb-remote-test/$worktree" >&2
ssh -o BatchMode=yes "$host" "mkdir -p pb-remote-test/$worktree"
rsync -az --delete -e 'ssh -o BatchMode=yes' "$stage"/ "$host:pb-remote-test/$worktree/"

# The quoted heredoc is the remote script; arguments are passed as positional parameters (no re-parsing).
echo "remote-test: $host: go test -p 2 $* ($module)" >&2
printf -v quoted ' %q' "$@"
exec ssh -o BatchMode=yes -o ServerAliveInterval=30 "$host" \
  "cap=; systemctl --user show-environment >/dev/null 2>&1 && cap='systemd-run --user --scope -q -p MemoryMax=60%'; GITHUB_SHA=$(git rev-parse HEAD) \$cap flock -w 3600 \$HOME/pb-remote-test/host.lock bash -s -- $module $worktree$quoted" <<'REMOTE'
set -euo pipefail
umask 022 # a host default of 002 makes every t.TempDir group-writable, which the daemon socket check refuses
module=$1 worktree=$2
shift 2
if [ "$module" = cli ] && ! sudo -n true 2>/dev/null; then
  echo "remote-test: warning: $(hostname) has no passwordless sudo; cli root-updater and isolated-filesystem tests will fail here, run cli on tools/ci-test.sh or a host with passwordless sudo" >&2
fi
export PATH=$HOME/.local/go1.27.4/bin:$HOME/go-sdk-1.27.4/bin:$PATH GOTOOLCHAIN=local
# Keep each job’s build cache separate from other jobs and the host default.
export GOCACHE=$HOME/.cache/go-build-$worktree
cd "$HOME/pb-remote-test/$worktree/$module"
go version
# A host with ~/.pb-test-pg-dsn runs PostgreSQL suites like CI: each package gets its own fresh database.
case $module in server | edge | helper) ;; *) exec nice go test -p 2 "$@" ;; esac # no database suites elsewhere
[ -f "$HOME/.pb-test-pg-dsn" ] || exec nice go test -p 2 "$@"
# One go test run: tools/pg-test-exec.sh (git archive ships tools/) gives each package a fresh database, packages in parallel.
PAPERBOAT_TEST_DATABASE_DSN=$(cat "$HOME/.pb-test-pg-dsn") exec nice go test -p 2 -exec "$HOME/pb-remote-test/$worktree/tools/pg-test-exec.sh" "$@"
REMOTE
