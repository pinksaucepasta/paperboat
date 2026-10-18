#!/usr/bin/env bash
# Run exactly the checks that have ever passed on TARGET (green/TARGET.list) on a build.
#   tools/device-smoke/green.sh TARGET --expect-version dev-<hash> [smoke.sh options]
# No build becomes the base on a device until this passes on it.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
[ $# -ge 1 ] || { sed -n '2,4p' "$0" >&2; exit 2; }
target=$1; shift
[ -r "$HERE/green/$target.list" ] || { echo "no green list for $target" >&2; exit 2; }
exec "$HERE/smoke.sh" --targets "$target" --only-list "$HERE/green/$target.list" "$@"
