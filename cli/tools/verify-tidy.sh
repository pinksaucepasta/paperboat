#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
top=$(git -C "$root" rev-parse --show-toplevel)
module=${root#"$top"/}
work=$(mktemp -d "${TMPDIR:-/tmp}/paperboat-tidy.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM

# cli/go.mod replaces sibling modules (../shared), so the whole workspace is copied, not just the module.
mkdir "$work/repository"
(
	cd "$top"
	git ls-files -z --cached --others --exclude-standard |
		xargs -0 -n 1 sh -c 'test ! -e "$1" || printf "%s\0" "$1"' sh >"$work/files"
	tar --null -T "$work/files" -cf "$work/repository.tar"
)
tar -xf "$work/repository.tar" -C "$work/repository"

cd "$work/repository/$module"
GOTOOLCHAIN=local go mod tidy

status=0
for file in "$module/go.mod" "$module/go.sum" go.work.sum; do
  if ! cmp -s "$top/$file" "$work/repository/$file"; then
    echo "tidy: $file is stale; run make tidy" >&2
    diff -u "$top/$file" "$work/repository/$file" || true
    status=1
  fi
done
[ "$status" -eq 0 ] || exit "$status"

echo "tidy: module files are current"
