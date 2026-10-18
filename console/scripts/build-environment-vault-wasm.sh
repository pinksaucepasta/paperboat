#!/usr/bin/env bash
# Builds the browser ENV vault WASM from cli/cmd/environmentvault-wasm into public/environment-vault,
# with the wasm_exec.js of the same toolchain. TinyGo (about 1 MB gzipped) when it is on PATH,
# otherwise standard Go (about 2.3 MB gzipped); the interop test in cmd/environmentvault-wasm
# checks either module against the native code. The build is skipped while the outputs are newer
# than their sources, so `next dev` starts fast.
set -euo pipefail

console_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cli_dir="$console_dir/../cli"
assets_dir="$console_dir/public/environment-vault"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/pb-environmentvault.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

command -v go >/dev/null || { echo "build-environment-vault-wasm: Go is required (see go.work)" >&2; exit 1; }
toolchain=go
command -v tinygo >/dev/null && toolchain=tinygo
if [ -f "$assets_dir/environment-vault.wasm" ] &&
  [ -z "$(find "$cli_dir/cmd/environmentvault-wasm" "$cli_dir/internal/environmente2ee" -name '*.go' -newer "$assets_dir/environment-vault.wasm" -print -quit)" ]; then
  exit 0
fi

if [ "$toolchain" = tinygo ]; then
  (cd "$cli_dir" && tinygo build -target wasm -no-debug -opt=z -o "$tmp/environment-vault.wasm" ./cmd/environmentvault-wasm 2>&1 |
    { grep -v 'Reserved registers' || true; })
  glue="$(tinygo env TINYGOROOT)/targets/wasm_exec.js"
else
  (cd "$cli_dir" && GOOS=js GOARCH=wasm go build -trimpath -ldflags='-s -w' -o "$tmp/environment-vault.wasm" ./cmd/environmentvault-wasm)
  glue="$(go env GOROOT)/lib/wasm/wasm_exec.js"
fi
install -m 0644 "$tmp/environment-vault.wasm" "$assets_dir/environment-vault.wasm"
install -m 0644 "$glue" "$assets_dir/wasm_exec.js"
