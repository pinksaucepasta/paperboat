#!/usr/bin/env bash
set -euo pipefail

package_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cli_dir="$(cd -- "$package_dir/../.." && pwd)"
workspace_dir="$(cd -- "$cli_dir/../.." && pwd)"
dashboard_dir="$workspace_dir/paperboat-dashboard"
assets_dir="$dashboard_dir/public/terminal"
temporary_dir="$(mktemp -d "${TMPDIR:-/tmp}/paperboat-browser-terminal.XXXXXX")"
trap 'rm -rf "$temporary_dir"' EXIT

test -d "$dashboard_dir" || { echo "paperboat-dashboard was not found beside paperboat-oss" >&2; exit 1; }
(cd "$cli_dir" && GOOS=js GOARCH=wasm go build -trimpath -ldflags='-s -w' -o "$temporary_dir/browserterminal.wasm" ./cmd/browserterminal-wasm)
install -d "$assets_dir"
install -m 0644 "$temporary_dir/browserterminal.wasm" "$assets_dir/browserterminal.wasm"
# Serve the Go runtime from the same Go toolchain used to build the WASM module.
install -m 0644 "$(go env GOROOT)/lib/wasm/wasm_exec.js" "$assets_dir/wasm_exec.js"
