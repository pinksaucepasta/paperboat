#!/usr/bin/env bash
# Builds the browser ENV vault WASM from cli/cmd/environmentvault-wasm into public/environment-vault,
# with the wasm_exec.js of the same toolchain: standard Go (about 2.3 MB gzipped). Never TinyGo: its
# size-optimized, asyncify-instrumented code runs the vault password's Argon2id (64 MiB, 3 passes) so
# slowly that unlocking took minutes (2026-11-16: over 170 s, against 4 s with standard Go).
# The build is skipped while the outputs are newer than their sources (and this script), so
# `next dev` starts fast.
set -euo pipefail

console_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cli_dir="$console_dir/../cli"
assets_dir="$console_dir/public/environment-vault"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/pb-environmentvault.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

command -v go >/dev/null || { echo "build-environment-vault-wasm: Go is required (see go.work)" >&2; exit 1; }
if [ -f "$assets_dir/environment-vault.wasm" ] &&
  [ -z "$(find "$cli_dir/cmd/environmentvault-wasm" "$cli_dir/internal/environmente2ee" "$cli_dir/internal/recording" -name '*.go' -newer "$assets_dir/environment-vault.wasm" -print -quit)" ] &&
  [ "${BASH_SOURCE[0]}" -ot "$assets_dir/environment-vault.wasm" ]; then
  exit 0
fi

(cd "$cli_dir" && GOOS=js GOARCH=wasm go build -trimpath -ldflags='-s -w' -o "$tmp/environment-vault.wasm" ./cmd/environmentvault-wasm)
glue="$(go env GOROOT)/lib/wasm/wasm_exec.js"
install -m 0644 "$tmp/environment-vault.wasm" "$assets_dir/environment-vault.wasm"
install -m 0644 "$glue" "$assets_dir/wasm_exec.js"
