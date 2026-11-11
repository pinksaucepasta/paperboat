#!/bin/sh
set -eu

require_module() {
  path=$1
  version=$2
  actual=$(GOTOOLCHAIN=local go list -m -f '{{.Version}}' "$path")
  [ "$actual" = "$version" ] || {
    echo "dependency mismatch: $path is $actual, want $version" >&2
    exit 1
  }
}

require_module golang.org/x/crypto v0.58.1-0.20261009181718-2f1f834330b2
require_module github.com/realclientip/realclientip-go v1.0.0

echo "server dependencies: valid"
