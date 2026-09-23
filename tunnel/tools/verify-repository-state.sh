#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
mode=${1:-development}
case "$mode" in development|ci|release) ;; *) exit 64 ;; esac
grep -Eq '^FROM debian:bookworm-slim@sha256:[0-9a-f]{64}$' "$root/deploy/Dockerfile" || {
  echo "repository state: runtime image must be digest-pinned" >&2; exit 1;
}
if [ "$mode" = release ]; then
  [ -n "${GITHUB_REF_NAME:-}" ] || { echo "repository state: GITHUB_REF_NAME is required" >&2; exit 1; }
  [ -z "$(git -C "$root" status --short)" ] || { echo "repository state: release worktree is not clean" >&2; exit 1; }
fi
echo "repository state: $mode checks passed"
