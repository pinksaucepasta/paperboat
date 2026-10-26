#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
snapshot=$(mktemp -d "${TMPDIR:-/tmp}/paperboat-license.XXXXXX")

cleanup() {
	status=$?
	if ! cmp -s "$root/go.mod" "$snapshot/go.mod" || ! cmp -s "$root/go.sum" "$snapshot/go.sum"; then
		echo "license analysis changed go.mod or go.sum" >&2
		status=1
	fi
	rm -rf "$snapshot"
	exit "$status"
}
trap cleanup EXIT HUP INT TERM

cp "$root/go.mod" "$snapshot/go.mod"
cp "$root/go.sum" "$snapshot/go.sum"

cd "$root"
own_module=$(GOTOOLCHAIN=local go list -f '{{.Module.Path}}' ./cmd/pb)
test -n "$own_module" || { echo "cannot resolve the CLI's own module" >&2; exit 1; }
GOTOOLCHAIN=local go list -m -json all > "$snapshot/modules.json"
python3 "$root/tools/verify-license-exceptions.py" \
	"$root/upstream/license-exceptions.json" "$snapshot/modules.json" > "$snapshot/ignored"

# go-licenses still traverses dependencies of ignored packages. The trailing
# slash excludes our repository's packages, not similarly named external repos.
set -- --disallowed_types=forbidden,restricted,unknown "--ignore=$own_module/"
while IFS= read -r module; do
	set -- "$@" "--ignore=$module"
done < "$snapshot/ignored"

GOTOOLCHAIN=local go run github.com/google/go-licenses@v1.6.0 check \
	"$@" ./cmd/pb
