#!/usr/bin/env bash
# Builds the browser terminal WASM module from cli/cmd/browserterminal-wasm
# into public/terminal, with the wasm_exec.js of the same Go toolchain.
set -euo pipefail

console_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cli_dir="$console_dir/../cli"
assets_dir="$console_dir/public/terminal"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/pb-browserterminal.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

command -v go >/dev/null || { echo "build-terminal-wasm: Go is required (see go.work)" >&2; exit 1; }
(cd "$cli_dir" && GOOS=js GOARCH=wasm go build -trimpath -ldflags='-s -w' \
  -o "$tmp/browserterminal.wasm" ./cmd/browserterminal-wasm)
install -m 0644 "$tmp/browserterminal.wasm" "$assets_dir/browserterminal.wasm"
install -m 0644 "$(go env GOROOT)/lib/wasm/wasm_exec.js" "$assets_dir/wasm_exec.js"
