#!/usr/bin/env bash
# Runs as root on the release host. Adds one immutable self-host version to the live release tree
# and then points the one-liner at it. The tree is shared with the CLI/TUF publisher, so this takes
# the same lock; a version directory is complete before `install` switches to it.
set -euo pipefail
if [[ $# -ne 4 ]]; then
  echo "usage: publish-selfhost-origin.sh STAGED_BUNDLE RELEASE_ROOT VERSION EXPECTED_SHA256" >&2
  exit 2
fi
bundle=$1 root=$2 version=$3 expected=$4
keep=8
[[ "$version" =~ ^20[0-9]{2}\.[0-9]{2}\.[0-9]{2}\.(0|[1-9][0-9]*)$ ]] || { echo "invalid release version" >&2; exit 1; }
[[ "$expected" =~ ^[a-f0-9]{64}$ ]] || { echo "invalid bundle digest" >&2; exit 1; }
[[ "$root" == /* && -d "$root" && ! -L "$root" && -d "$root/staging" && ! -L "$root/staging" ]] || { echo "release root is unavailable" >&2; exit 1; }
[[ -f "$bundle" && ! -L "$bundle" ]] || { echo "staged bundle is unavailable" >&2; exit 1; }
[[ "$(sha256sum "$bundle" | awk '{print $1}')" == "$expected" ]] || { echo "staged bundle digest mismatch" >&2; exit 1; }

exec 9>"$root/.publish-tuf-origin.lock"
flock -x 9
live=$root/current
[[ -d "$live" && ! -L "$live" ]] || { echo "live release is unavailable" >&2; exit 1; }

work=$(mktemp -d "$root/staging/selfhost.XXXXXX")
trap 'rm -rf -- "$work"' EXIT
required=(install manifest.json paperboat-selfhost-linux-amd64.tar.gz paperboat-selfhost-linux-arm64.tar.gz)
[[ "$(tar -tzf "$bundle" | sort)" == "$(printf '%s\n' "${required[@]}" | sort)" ]] || { echo "bundle must contain exactly: ${required[*]}" >&2; exit 1; }
tar -xzf "$bundle" --no-same-owner --no-same-permissions -C "$work"
for name in "${required[@]}"; do [[ -f "$work/$name" && ! -L "$work/$name" && -s "$work/$name" ]] || { echo "bundle member $name is not a non-empty regular file" >&2; exit 1; }; done
grep -q "\"version\":\"$version\"" "$work/manifest.json" || { echo "manifest does not describe $version" >&2; exit 1; }
for arch in amd64 arm64; do
  grep -q "\"linux-$arch\":{\"sha256\":\"$(sha256sum "$work/paperboat-selfhost-linux-$arch.tar.gz" | awk '{print $1}')\"" "$work/manifest.json" || { echo "manifest digest does not match the $arch package" >&2; exit 1; }
done

selfhost=$live/selfhost
target=$selfhost/versions/$version
mkdir -p "$selfhost/versions"
if [[ -e "$target" ]]; then
  # Re-running the same publication is a no-op; different bytes under a published version never replace it.
  for name in "${required[@]}"; do cmp -s "$work/$name" "$target/$name" || { echo "version $version is already published with different bytes" >&2; exit 1; }; done
else
  mkdir -p "$work/version"
  mv "$work"/install "$work"/manifest.json "$work"/paperboat-selfhost-*.tar.gz "$work/version/"
  cp "$work/version/install" "$work/install" && cp "$work/version/manifest.json" "$work/manifest.json"
  mv "$work/version" "$target"
fi
if [[ ! -e "$work/install" ]]; then cp "$target/install" "$work/install"; cp "$target/manifest.json" "$work/manifest.json"; fi
owner=${PAPERBOAT_RELEASE_OWNER:-501:root}
if [[ "$(id -u)" -eq 0 ]]; then chown -R "$owner" "$selfhost"; fi
chmod 0755 "$work/install"; chmod 0644 "$work/manifest.json"
if [[ "$(id -u)" -eq 0 ]]; then chown "$owner" "$work/install" "$work/manifest.json"; fi
# Files appear atomically; the version directory they point at already exists.
mv -f "$work/install" "$selfhost/install.new" && mv -f "$selfhost/install.new" "$selfhost/install"
mv -f "$work/manifest.json" "$selfhost/manifest.json.new" && mv -f "$selfhost/manifest.json.new" "$selfhost/manifest.json"

# Bound the tree: the CLI publisher refuses more than 32 versions. Never remove the active one.
mapfile -t old < <(ls -1 "$selfhost/versions" | sort -V | head -n -"$keep")
for name in "${old[@]}"; do [[ -n "$name" && "$name" != "$version" ]] && rm -rf -- "$selfhost/versions/$name"; done
echo "Published self-host $version."
