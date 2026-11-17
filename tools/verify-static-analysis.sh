#!/bin/sh
# usage: tools/verify-static-analysis.sh <module>   (cli or server)
set -eu
. "$(dirname -- "$0")/verify-lib.sh"
module_setup "$@"
guard_go_files "static analysis"

case $module in
cli) deadcode_packages='./cmd/pb ./internal/...' ;;
server) deadcode_packages='./cmd/... ./internal/... ./tools/...' ;;
*) echo "static analysis: no deadcode roots defined for $module" >&2; exit 64 ;;
esac

cd "$root"
GOTOOLCHAIN=local go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 -checks='all,-ST1000,-ST1005' ./...
# shellcheck disable=SC2086 # the package list is deliberately word-split
GOTOOLCHAIN=local go run golang.org/x/tools/cmd/deadcode@v0.49.0 -test $deadcode_packages
