#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
require_module() {
  actual=$(cd "$root" && GOTOOLCHAIN=local go list -m -f '{{.Version}}' "$1")
  [ "$actual" = "$2" ] || { echo "dependency mismatch: $1 is $actual, want $2" >&2; exit 1; }
}
require_module github.com/quic-go/quic-go v0.61.0
require_module github.com/realclientip/realclientip-go v1.0.0
echo "tunnel dependencies: valid"
