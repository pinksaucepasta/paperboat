#!/bin/sh
# Build only the requested Linux artifact in an explicit ignored output directory.
set -eu
if [ "$#" -ne 3 ]; then echo 'Usage: package-selfhost.sh VERSION amd64|arm64 ABSOLUTE_OUTPUT_DIR' >&2; exit 2; fi
version=$1
arch=$2
output=$3
case "$arch" in amd64|arm64) ;; *) echo 'Architecture must be amd64 or arm64.' >&2; exit 2;; esac
case "$output" in /*/.build/*) ;; *) echo 'Output must be an absolute path beneath an ignored .build directory.' >&2; exit 2;; esac
case "$version" in *[!A-Za-z0-9._-]*|'') echo 'Invalid version.' >&2; exit 2;; esac
repo=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
mkdir -p "$output"
package_tmp=$(mktemp -d "$output/.package.XXXXXX")
trap 'rm -rf "$package_tmp"' EXIT HUP INT TERM
(cd "$repo/relay" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$package_tmp/pbh" ./cmd/pbh)
(cd "$repo/relay" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$package_tmp/paperboat-relay" ./cmd/paperboat-relay)
(cd "$repo/tunnel" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$package_tmp/paperboat-tunnel" ./cmd/paperboat-tunnel)
cp "$repo/tunnel/LICENSE" "$package_tmp/LICENSE"
(cd "$repo/relay" && cp "$(go list -m -f '{{.Dir}}' tailscale.com)/LICENSE" "$package_tmp/tailscale.LICENSE")
printf '{"version":"%s","os":"linux","arch":"%s"}\n' "$version" "$arch" > "$package_tmp/manifest.json"
tar -czf "$output/paperboat-selfhost-linux-$arch.tar.gz" -C "$package_tmp" pbh paperboat-relay paperboat-tunnel LICENSE tailscale.LICENSE manifest.json
(cd "$output" && sha256sum "paperboat-selfhost-linux-$arch.tar.gz" > SHA256SUMS)
printf '%s\n' "$output/paperboat-selfhost-linux-$arch.tar.gz"
