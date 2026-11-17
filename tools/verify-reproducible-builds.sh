#!/bin/sh
# usage: tools/verify-reproducible-builds.sh <module>   (cli: VERSION, COMMIT and PROTOCOL_VERSION required; or server)
set -eu
. "$(dirname -- "$0")/verify-lib.sh"
module_setup "$@"

case $module in
cli)
	version=${VERSION:?VERSION is required}
	commit=${COMMIT:?COMMIT is required}
	protocol_version=${PROTOCOL_VERSION:?PROTOCOL_VERSION is required}
	package=./cmd/pb
	prefix=pb
	targets='darwin/arm64 linux/amd64 linux/arm64'
	buildinfo=github.com/pinksaucepasta/paperboat/cli/internal/buildinfo
	ldflags="-s -w -X $buildinfo.Version=$version -X $buildinfo.Commit=$commit -X $buildinfo.ProtocolVersion=$protocol_version"
	;;
server)
	package=./cmd/paperboat-server
	prefix=paperboat-server
	targets='linux/amd64 linux/arm64'
	ldflags='-s -w'
	;;
*) echo "reproducible builds: no entry point defined for $module" >&2; exit 64 ;;
esac

work=$(mktemp -d "${TMPDIR:-/tmp}/paperboat-reproducible.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM

if command -v sha256sum >/dev/null 2>&1; then
  checksum() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  checksum() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  echo "reproducible builds: sha256sum or shasum is required" >&2
  exit 2
fi

build_set() {
  output_dir=$1
  mkdir -p "$output_dir"
  for target in $targets; do
    os=${target%/*}
    arch=${target#*/}
    (cd "$root" && CGO_ENABLED=0 GOOS=$os GOARCH=$arch GOTOOLCHAIN=local SOURCE_DATE_EPOCH=0 \
      go build -buildvcs=false -trimpath -ldflags "$ldflags" -o "$output_dir/$prefix-$os-$arch" "$package")
  done
}

build_set "$work/first"
build_set "$work/second"

for first in "$work/first"/*; do
  name=${first##*/}
  cmp -s "$first" "$work/second/$name" || {
    echo "reproducible builds: $name differs between builds" >&2
    exit 1
  }
  echo "$(checksum "$first")  $name"
done

echo "reproducible builds: all supported $module artifacts are identical"
