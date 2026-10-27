#!/usr/bin/env bash
# Run one module's Go tests for the committed HEAD on a spare VM instead of the shared build host.
#   tools/remote-test.sh <host> <module> [go test args...]
#   tools/remote-test.sh pbvm2 cli -run TestExec ./cmd/pb
#   tools/remote-test.sh pbvm3 shared            # go test -p 2 ./...
# <host> is an ssh alias of a VM with Go 1.27.3 in ~/.local/go1.27.3 (pbvm2, pbvm3). The VMs have 3-4 GB
# of RAM, so the host runs one test job at a time (a host-wide lock; later jobs wait) with `nice go test -p 2`.
# Only git-tracked, committed files are tested: that is what `git archive HEAD` ships. No PostgreSQL is
# available there, so PostgreSQL suites skip, and there is no C compiler, so no -race; use tools/ci-test.sh
# for those.
set -euo pipefail

usage() { sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
[ $# -ge 2 ] || usage
host=$1 module=$2
shift 2
case $module in shared | cli | relay | tunnel | helper | server) ;; *) echo "remote-test: unknown module '$module'" >&2; usage ;; esac

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
  "flock -w 3600 \$HOME/pb-remote-test/host.lock bash -s -- $module $lane$quoted" <<'REMOTE'
set -euo pipefail
module=$1 lane=$2
shift 2
export PATH=$HOME/.local/go1.27.3/bin:$PATH GOTOOLCHAIN=local
cd "$HOME/pb-remote-test/$lane/$module"
go version
exec nice go test -p 2 "$@"
REMOTE
