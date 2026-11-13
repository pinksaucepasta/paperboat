#!/usr/bin/env bash
# Run one module's Go tests for the committed HEAD on a spare VM instead of the shared build host.
#   tools/remote-test.sh <host> <module> [go test args...]
#   tools/remote-test.sh pbvm2 cli -run TestExec ./cmd/pb
#   tools/remote-test.sh pbvm3 shared            # go test -p 2 ./...
# <host> is an ssh alias with Go 1.27.4 in ~/.local/go1.27.4 or ~/go-sdk-1.27.4 (pbvm2, pbvm3, ubuntu@anvit). The VMs have 3-4 GB
# of RAM, so the host runs one test job at a time (a host-wide lock; later jobs wait) with `nice go test -p 2`.
# Only git-tracked, committed files are tested: that is what `git archive HEAD` ships. No PostgreSQL is
# on pbvm2/pbvm3, so PostgreSQL suites skip there; anvit has one (~/.pb-test-pg-dsn), so use anvit for server.
# The synced tree has no .git, so the full HEAD goes over as GITHUB_SHA (tools/tuf-repository reads it for its release source commit).
# gcc is installed on all three, so -race works.
set -euo pipefail

usage() { sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
[ $# -ge 2 ] || usage
host=$1 module=$2
shift 2
case $module in shared | cli | relay | edge | helper | server) ;; *) echo "remote-test: unknown module '$module'" >&2; usage ;; esac

root=$(git rev-parse --show-toplevel)
cd "$root"
[ -d "$module" ] || { echo "remote-test: no $module/ in $root" >&2; exit 2; }
if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
  echo "remote-test: note: uncommitted changes are NOT tested (only HEAD $(git rev-parse --short HEAD))" >&2
fi

wanted_pkg=0
for a in "$@"; do case $a in ./* | ...) wanted_pkg=1 ;; esac; done
[ "$wanted_pkg" = 1 ] || set -- "$@" ./...

# One sync tree per worktree, so two worktrees never overwrite each other's sources; this script
# serializes against itself per worktree and host.
lane=$(printf %s "${root##*/}" | tr -c 'A-Za-z0-9._-' _)
exec 8>"${TMPDIR:-/tmp}/pb-remote-test-$host-$lane.lock"
flock 8

stage=$(mktemp -d "${TMPDIR:-/tmp}/pb-remote-test.XXXXXX")
trap 'rm -rf "$stage"' EXIT
git archive HEAD -- . ':!console' ':!marketing' | tar x -C "$stage"
sha=$(git rev-parse --short HEAD)

echo "remote-test: syncing $sha to $host:pb-remote-test/$lane" >&2
ssh -o BatchMode=yes "$host" "mkdir -p pb-remote-test/$lane"
rsync -az --delete -e 'ssh -o BatchMode=yes' "$stage"/ "$host:pb-remote-test/$lane/"

# The quoted heredoc is the remote script; arguments are passed as positional parameters (no re-parsing).
echo "remote-test: $host: go test -p 2 $* ($module)" >&2
printf -v quoted ' %q' "$@"
exec ssh -o BatchMode=yes -o ServerAliveInterval=30 "$host" \
  "GITHUB_SHA=$(git rev-parse HEAD) flock -w 3600 \$HOME/pb-remote-test/host.lock bash -s -- $module $lane$quoted" <<'REMOTE'
set -euo pipefail
module=$1 lane=$2
shift 2
export PATH=$HOME/.local/go1.27.4/bin:$HOME/go-sdk-1.27.4/bin:$PATH GOTOOLCHAIN=local
cd "$HOME/pb-remote-test/$lane/$module"
go version
# A host with ~/.pb-test-pg-dsn (anvit) runs PostgreSQL suites like CI: each package gets its own fresh database.
case $module in server | edge | helper) ;; *) exec nice go test -p 2 "$@" ;; esac # no database suites elsewhere
[ -f "$HOME/.pb-test-pg-dsn" ] || exec nice go test -p 2 "$@"
base=$(cat "$HOME/.pb-test-pg-dsn") pkgs=() args=() failed=0
for a in "$@"; do case $a in ./* | ...) pkgs+=("$a") ;; *) args+=("$a") ;; esac; done
mod=$(go list -f "{{.Module.Path}}" "${pkgs[@]}" | sed -n 1p)
for pkg in $(go list "${pkgs[@]}"); do
  db="lane_$(sed -e "s|^$mod/||" -e 's|[^A-Za-z0-9]|_|g' <<<"$pkg")_test"
  psql "${base%%lane_test*}postgres?sslmode=disable" -qc "drop database if exists \"$db\" with (force)" -c "create database \"$db\"" || { failed=1; continue; }
  PAPERBOAT_TEST_DATABASE_DSN="${base/lane_test\?/$db?}" nice go test "${args[@]}" "$pkg" || failed=1
done
exit $failed
REMOTE
