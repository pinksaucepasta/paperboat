#!/bin/sh
# usage: tools/verify-licenses.sh <module>   (cli or server)
set -eu
. "$(dirname -- "$0")/verify-lib.sh"
module_setup "$@"
guard_go_files "license analysis"

cd "$root"
case $module in
cli)
	own_module=$(GOTOOLCHAIN=local go list -f '{{.Module.Path}}' ./cmd/pb)
	test -n "$own_module" || { echo "cannot resolve the CLI's own module" >&2; exit 1; }
	GOTOOLCHAIN=local go list -m -json all > "$snapshot/modules.json"
	python3 "$root/tools/verify-license-exceptions.py" \
		"$root/upstream/license-exceptions.json" "$snapshot/modules.json" > "$snapshot/ignored"

	# go-licenses still traverses dependencies of ignored packages. The trailing
	# slash excludes our repository's packages, not similarly named external repos.
	set -- --disallowed_types=forbidden,restricted,unknown "--ignore=$own_module/"
	while IFS= read -r ignored_module; do
		set -- "$@" "--ignore=$ignored_module"
	done < "$snapshot/ignored"
	GOTOOLCHAIN=local go run github.com/google/go-licenses@v1.6.0 check "$@" ./cmd/pb
	;;
server)
	GOTOOLCHAIN=local go run github.com/google/go-licenses@v1.6.0 check \
		--disallowed_types=forbidden,restricted,unknown \
		--ignore github.com/pinksaucepasta/paperboat \
		./cmd/paperboat-server
	;;
*) echo "license analysis: no entry point defined for $module" >&2; exit 64 ;;
esac
