#!/bin/sh
# Decode the server's signed artifacts with the oldest supported official release (PLAN 3b rollout).
# Usage: tools/compat/run.sh   (from the repo root; needs the release tag fetched).
set -eu
root=$(git rev-parse --show-toplevel)
tag=$(cat "$root/tools/compat/OLDEST_OFFICIAL")
if ! git -C "$root" rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
	echo "compat: tag $tag not fetched; skipped (git fetch --tags to run it)" >&2
	exit 0
fi
work=$(mktemp -d)
trap 'git -C "$root" worktree remove --force "$work/old" >/dev/null 2>&1 || true; rm -rf "$work"' EXIT
mkdir -p "$work/artifacts"
(cd "$root/server" && COMPAT_ARTIFACTS_DIR="$work/artifacts" go test ./internal/compat ./internal/peersessions -run 'TestEmitCompat' -count=1)
git -C "$root" worktree add -q --detach "$work/old" "$tag"
for f in "$root"/tools/compat/*_compat_test.go; do
	target=$(sed -n 's|^// compat:target ||p' "$f" | head -1)
	cp "$f" "$work/old/$target/"
done
(cd "$work/old/cli" && COMPAT_ARTIFACTS_DIR="$work/artifacts" go test -count=1 -run 'TestCompat' ./internal/hostruntime/auth ./internal/hostruntime/connector ./internal/peertransport/tailnet ./internal/resolver)
echo "compat: server artifacts decode with official $tag"
