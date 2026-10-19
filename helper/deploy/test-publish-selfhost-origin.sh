#!/usr/bin/env bash
# Exercises publish-selfhost-origin.sh and render-installer.sh against a temporary release root.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
export PAPERBOAT_RELEASE_OWNER=
fail() { echo "FAIL: $*" >&2; exit 1; }
make_bundle() { # version content -> $tmp/bundle-<version>.tgz
  local v=$1 c=$2 d; d=$(mktemp -d "$tmp/pkg.XXXX")
  for a in amd64 arm64; do printf '%s-%s' "$c" "$a" > "$d/paperboat-selfhost-linux-$a.tar.gz"; done
  (cd "$d" && sha256sum paperboat-selfhost-linux-*.tar.gz > SHA256SUMS)
  "$here/render-installer.sh" "$v" "https://get.example.test/selfhost/versions/$v" https://docker.example.test/install "$d" >/dev/null
  tar -czf "$tmp/bundle-$v.tgz" -C "$d" install manifest.json paperboat-selfhost-linux-amd64.tar.gz paperboat-selfhost-linux-arm64.tar.gz
}
publish() { bash "$here/publish-selfhost-origin.sh" "$tmp/bundle-$1.tgz" "$tmp/root" "$1" "$(sha256sum "$tmp/bundle-$1.tgz" | awk '{print $1}')"; }
mkdir -p "$tmp/root/current/tuf" "$tmp/root/staging"

make_bundle 2026.10.19.1 one; publish 2026.10.19.1 >/dev/null
[[ "$(ls "$tmp/root/current/selfhost" | sort | tr '\n' ' ')" == "install manifest.json versions " ]] || fail "unexpected top-level selfhost entries"
grep -q 'selfhost/versions/2026.10.19.1/paperboat-selfhost-linux-arm64.tar.gz' "$tmp/root/current/selfhost/install" || fail "installer does not point at the version"
! grep -q '{{' "$tmp/root/current/selfhost/install" || fail "unrendered installer value"
[[ -z "$(ls -A "$tmp/root/staging")" ]] || fail "staging not cleaned"

publish 2026.10.19.1 >/dev/null || fail "identical republish must be a no-op"
make_bundle 2026.10.19.1 different
if publish 2026.10.19.1 2>/dev/null; then fail "published version bytes were replaced"; fi
grep -q 'one-amd64' <(cat "$tmp/root/current/selfhost/versions/2026.10.19.1/paperboat-selfhost-linux-amd64.tar.gz") || fail "published bytes changed"

for i in 2 3 4 5 6 7 8 9 10 11; do make_bundle "2026.10.19.$i" "v$i"; publish "2026.10.19.$i" >/dev/null; done
[[ "$(ls "$tmp/root/current/selfhost/versions" | wc -l)" -eq 8 ]] || fail "versions are not bounded"
grep -q 'versions/2026.10.19.11/' "$tmp/root/current/selfhost/install" || fail "install does not point at the latest version"
[[ -d "$tmp/root/current/selfhost/versions/2026.10.19.11" && ! -e "$tmp/root/current/selfhost/versions/2026.10.19.1" ]] || fail "pruning removed the wrong versions"

# Refusals leave the live tree unchanged.
before=$(cd "$tmp/root/current" && find . -type f -exec sha256sum {} + | sort | sha256sum)
bad=$tmp/bundle-bad.tgz; d=$(mktemp -d "$tmp/bad.XXXX"); echo x > "$d/extra"; tar -czf "$bad" -C "$d" extra
if bash "$here/publish-selfhost-origin.sh" "$bad" "$tmp/root" 2026.10.19.12 "$(sha256sum "$bad" | awk '{print $1}')" 2>/dev/null; then fail "accepted a bundle with the wrong members"; fi
if bash "$here/publish-selfhost-origin.sh" "$tmp/bundle-2026.10.19.11.tgz" "$tmp/root" 2026.10.19.12 "$(printf 0%.0s {1..64})" 2>/dev/null; then fail "accepted a wrong digest"; fi
make_bundle 2026.10.19.13 x
if bash "$here/publish-selfhost-origin.sh" "$tmp/bundle-2026.10.19.13.tgz" "$tmp/root" 2026.10.19.14 "$(sha256sum "$tmp/bundle-2026.10.19.13.tgz" | awk '{print $1}')" 2>/dev/null; then fail "accepted a manifest for another version"; fi
[[ "$before" == "$(cd "$tmp/root/current" && find . -type f -exec sha256sum {} + | sort | sha256sum)" ]] || fail "a refused publication changed the live tree"
echo "PASS publish-selfhost-origin"
