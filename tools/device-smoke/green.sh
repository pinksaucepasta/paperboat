#!/usr/bin/env bash
# Run exactly the checks that have ever passed on TARGET (green/TARGET.list) on a build.
#   tools/device-smoke/green.sh TARGETS --expect-version dev-<hash> [smoke.sh options]
# TARGETS is one device or a comma-separated pair.
# No build becomes the base on a device until this passes on it.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
[ $# -ge 1 ] || { sed -n '2,4p' "$0" >&2; exit 2; }
target=$1; shift
green=$(python3 "$HERE/dev_targets.py" green_dir)
list="${green:-$HERE/green}/${target//,/+}.list"
[ -r "$list" ] || { echo "no green list for $target" >&2; exit 2; }
exec "$HERE/smoke.sh" --targets "$target" --only-list "$list" "$@"
