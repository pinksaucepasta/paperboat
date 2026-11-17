#!/bin/sh
# usage: tools/verify-vulnerabilities.sh <module>
set -eu
. "$(dirname -- "$0")/verify-lib.sh"
module_setup "$@"
guard_go_files "vulnerability analysis"

cd "$root"
GOTOOLCHAIN=local go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
