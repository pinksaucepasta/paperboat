#!/bin/sh
# Render the one-liner installer for one version from the packages built by package-selfhost.sh.
set -eu
if [ "$#" -ne 4 ]; then echo 'Usage: render-installer.sh VERSION HTTPS_DOWNLOAD_BASE_URL HTTPS_DOCKER_INSTALL_URL ABSOLUTE_PACKAGE_DIR' >&2; exit 2; fi
version=$1
base_url=${2%/}
docker_url=$3
dir=$4
case "$version" in *[!0-9.]*|'') echo 'Invalid version.' >&2; exit 2;; esac
for url in "$base_url" "$docker_url"; do
 case "$url" in https://*) ;; *) echo 'Download and Docker installer URLs must use HTTPS.' >&2; exit 2;; esac
 case "$url" in *[!A-Za-z0-9:/._-]*) echo 'URL contains unsupported characters.' >&2; exit 2;; esac
done
case "$dir" in /*) ;; *) echo 'Package directory must be absolute.' >&2; exit 2;; esac
template=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)/install.sh
digest() {
 line=$(grep -E "^[0-9a-f]{64}  paperboat-selfhost-linux-$1\.tar\.gz\$" "$dir/SHA256SUMS" || true)
 [ -n "$line" ] || { echo "SHA256SUMS has no entry for linux-$1; run package-selfhost.sh for amd64 and arm64." >&2; exit 1; }
 printf '%s\n' "${line%% *}"
}
amd64=$(digest amd64)
arm64=$(digest arm64)
(cd "$dir" && sha256sum --check --status SHA256SUMS) || { echo "Packages do not match SHA256SUMS; build both with package-selfhost.sh in $dir." >&2; exit 1; }
sed -e "s|{{PACKAGE_URL_AMD64}}|$base_url/paperboat-selfhost-linux-amd64.tar.gz|" \
    -e "s|{{PACKAGE_SHA256_AMD64}}|$amd64|" \
    -e "s|{{PACKAGE_URL_ARM64}}|$base_url/paperboat-selfhost-linux-arm64.tar.gz|" \
    -e "s|{{PACKAGE_SHA256_ARM64}}|$arm64|" \
    -e "s|{{DOCKER_INSTALL_URL}}|$docker_url|" "$template" > "$dir/install"
chmod 0755 "$dir/install"
if grep -q '{{' "$dir/install"; then echo 'Unrendered installer value.' >&2; rm -f "$dir/install"; exit 1; fi
printf '%s\n' "$dir/install"
