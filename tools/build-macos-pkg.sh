#!/bin/sh
set -eu

usage() {
  echo "usage: $0 --binary FILE --output FILE --version VERSION [--signing-identity IDENTITY]" >&2
  exit 64
}

binary=
output=
version=
identity=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --binary|--output|--version|--signing-identity)
      [ "$#" -ge 2 ] || usage
      case "$1" in
        --binary) binary=$2 ;;
        --output) output=$2 ;;
        --version) version=$2 ;;
        --signing-identity) identity=$2 ;;
      esac
      shift 2
      ;;
    *) usage ;;
  esac
done

[ -f "$binary" ] && [ ! -L "$binary" ] || { echo "macOS package input is missing or not a regular file" >&2; exit 1; }
[ -n "$output" ] || usage
[ -n "$version" ] || usage

case "$version" in
  20[0-9][0-9].[0-9][0-9].[0-9][0-9].* ) ;;
  *) echo "invalid release version: $version" >&2; exit 1 ;;
esac

output_dir=$(dirname -- "$output")
mkdir -p "$output_dir"
if [ -e "$output" ] || [ -L "$output" ]; then
  echo "refusing to overwrite release package: $output" >&2
  exit 1
fi

root=$(mktemp -d "${TMPDIR:-/tmp}/paperboat-pkg.XXXXXX")
unsigned="$root/pb-darwin-arm64-unsigned.pkg"
cleanup() { rm -rf "$root"; }
trap cleanup EXIT HUP INT TERM

payload="$root/payload"
cli_payload="$payload/usr/local/bin/pb"
canonical_helper=/Library/PrivilegedHelperTools/Paperboat/bin/pb
helper_payload="$payload$canonical_helper"
mkdir -p "$(dirname -- "$cli_payload")" "$(dirname -- "$helper_payload")"
install -m 0755 "$binary" "$helper_payload"

# macOS refuses Go's linker-only signature when launchd starts the executable
# from a privileged helper location. TUF authenticates the development release
# bytes; replace that linker signature with a complete ad-hoc Mach-O signature
# so the installed hostd/updater can run when Developer ID material is absent.
# Sign the single canonical executable used by the CLI and launchd services.
# The CLI path is an exact absolute link so every invocation resolves to the
# same file that the updater atomically rotates.
codesign --force --sign - --timestamp=none "$helper_payload"
ln -s "$canonical_helper" "$cli_payload"
codesign --verify --strict "$helper_payload"

pkgbuild \
  --root "$payload" \
  --identifier dev.pprbt.paperboat \
  --version "$version" \
  --install-location / \
  --ownership recommended \
  "$unsigned"

if [ -n "$identity" ]; then
  productsign --sign "$identity" "$unsigned" "$output"
  pkgutil --check-signature "$output"
else
  mv "$unsigned" "$output"
fi
expanded="$root/expanded"
pkgutil --expand "$output" "$expanded"
test -f "$expanded/Payload" || { echo 'macOS package payload is missing' >&2; exit 1; }
