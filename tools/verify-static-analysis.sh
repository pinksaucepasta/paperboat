#!/bin/sh
# usage: tools/verify-static-analysis.sh <module>   (cli server edge relay helper shared)
#
# staticcheck and deadcode only see the files of one GOOS/GOARCH, so code that is used only by a
# windows/darwin/js file looks unused on linux. Each tool therefore runs once per platform the module
# ships on: ordinary findings are the union, "unused" findings (U1000, deadcode) count only when every
# platform reports them.
set -eu
. "$(dirname -- "$0")/verify-lib.sh"
module_setup "$@"
guard_go_files static-analysis

targets='linux/amd64 darwin/arm64 windows/amd64'
deadcode_filter=
wasm_packages= # packages built only for js/wasm: their unused code must be unused there too
case $module in
cli) deadcode_packages='./cmd/... ./internal/... ./tools/...' ; wasm_packages='./cmd/browserterminal-wasm ./cmd/environmentvault-wasm' ;; # tools/metric-schema calls telemetry.HistogramBuckets
server) deadcode_packages='./cmd/... ./internal/... ./tools/...'; targets=linux/amd64 ;;
edge) deadcode_packages='./cmd/... ./internal/... ./tools/...'; targets=linux/amd64 ;;
relay) deadcode_packages='./... github.com/pinksaucepasta/paperboat/helper/cmd/pbh'; deadcode_filter='^github.com/pinksaucepasta/paperboat/relay'; targets=linux/amd64 ;; # helper/cmd/pbh calls selfhost.RemoveLocal
helper) deadcode_packages='./cmd/... ./internal/...'; targets=linux/amd64 ;;
shared) deadcode_packages= ;; # a library: every exported function is a root for the other modules
*) echo "static analysis: no deadcode roots defined for $module" >&2; exit 64 ;;
esac

cd "$root"
work=$snapshot
# Built for this host once: `go run` under another GOOS would build a tool that cannot execute here.
GOBIN=$work/bin GOTOOLCHAIN=local go install honnef.co/go/tools/cmd/staticcheck@v0.8.2
GOBIN=$work/bin GOTOOLCHAIN=local go install golang.org/x/tools/cmd/deadcode@v0.50.0 # v0.49.0 crashes on programs that box *rand.Rand (golang/go#80973), which quic-go does
for target in $targets; do
	for tool in staticcheck deadcode; do
		case $tool in
		staticcheck) run="$work/bin/staticcheck -checks=all,-ST1000,-ST1005 ./..." ;;
		deadcode) [ -n "$deadcode_packages" ] || continue
			run="$work/bin/deadcode -test ${deadcode_filter:+-filter $deadcode_filter} $deadcode_packages" ;;
		esac
		rc=0
		# shellcheck disable=SC2086 # the argument list is deliberately word-split
		GOOS=${target%/*} GOARCH=${target#*/} GOTOOLCHAIN=local $run >"$work/$tool.${target%/*}-${target#*/}" 2>"$work/err" || rc=$?
		# 1 means findings; anything else is a tool or load failure that must not read as a pass.
		if [ "$rc" -gt 1 ] || { [ "$rc" -eq 1 ] && ! [ -s "$work/$tool.${target%/*}-${target#*/}" ]; }; then
			echo "$tool failed for $target (exit $rc):" >&2
			cat "$work/err" >&2
			exit 1
		fi
	done
done

if [ -n "$wasm_packages" ]; then
	rc=0
	# shellcheck disable=SC2086
	GOOS=js GOARCH=wasm GOTOOLCHAIN=local "$work/bin/staticcheck" -checks=all,-ST1000,-ST1005 $wasm_packages >"$work/wasm.out" 2>"$work/err" || rc=$?
	if [ "$rc" -gt 1 ] || { [ "$rc" -eq 1 ] && ! [ -s "$work/wasm.out" ]; }; then
		echo "staticcheck failed for js/wasm (exit $rc):" >&2
		cat "$work/err" >&2
		exit 1
	fi
fi

# Findings reported on every platform are unused code; the rest is the union over platforms.
every_platform() { # prints the lines present in all the files given
	cat "$@" | sort | uniq -c | awk -v n="$#" '$1 == n { sub(/^ *[0-9]+ /, ""); print }'
}
cat "$work"/staticcheck.* "$work/wasm.out" 2>/dev/null | grep -v '(U1000)$' | sort -u >"$work/findings" || true
for f in "$work"/staticcheck.*; do grep '(U1000)$' "$f" >"$f.unused" || true; done
every_platform "$work"/staticcheck.*.unused >"$work/unused"
if [ -n "$wasm_packages" ]; then
	grep '(U1000)$' "$work/wasm.out" >"$work/wasm.unused" || true
	for p in $wasm_packages; do echo "${p#./}/"; done >"$work/wasm.dirs"
	# a package built for js/wasm: unused only if js/wasm agrees (its js files may use what linux cannot see)
	awk -v dirsfile="$work/wasm.dirs" -v seenfile="$work/wasm.unused" \
		'FILENAME == dirsfile { dirs[$0]; next } FILENAME == seenfile { seen[$0]; next }
		{ keep = 1; for (d in dirs) if (index($0, d) == 1) keep = ($0 in seen); if (keep) print }' \
		"$work/wasm.dirs" "$work/wasm.unused" "$work/unused" >>"$work/findings"
else
	cat "$work/unused" >>"$work/findings"
fi
if ls "$work"/deadcode.* >/dev/null 2>&1; then every_platform "$work"/deadcode.* >>"$work/findings"; fi
if [ -s "$work/findings" ]; then
	cat "$work/findings" >&2
	exit 1
fi
