#!/bin/bash
# Runs the fixed install.sh as root in minimal container images that have no ssh-keygen, against a signed fake release
# (a throwaway key; the script under test is a copy that embeds its public half). The installer must install OpenSSH
# with the image's package manager, verify the signature, and refuse a tampered release. Needs docker and network (the
# package managers). usage: test-install-container.sh [image...]   (DOCKER="sudo docker" if needed)
set -euo pipefail
docker=${DOCKER:-docker}
images=("$@")
[ "${#images[@]}" -gt 0 ] || images=(python:3.12-slim python:3.12-alpine fedora:latest)
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

# The signed fake release: a pb that records its calls, both Linux CPUs, and a curl that serves it like GitHub does.
version=2026.11.18.1
mkdir -p "$work/release/$version" "$work/bin"
cat > "$work/release/$version/pb-linux-amd64" <<'PB'
#!/bin/sh
echo "$*" >> /tmp/pb-calls
if [ "$1" = install ]; then mkdir -p "$3"; cp "$0" "$3/pb"; chmod 0755 "$3/pb"; printf '{"ok":true,"data":{"executable":"%s"}}\n' "$3/pb"; fi
PB
cp "$work/release/$version/pb-linux-amd64" "$work/release/$version/pb-linux-arm64"
ssh-keygen -q -t ed25519 -N '' -C container-test -f "$work/key"
public=$(cut -d' ' -f1,2 "$work/key.pub")
sed "s|^allowed_signers=.*|allowed_signers='paperboat-release namespaces=\"paperboat-release\" $public'|" "$here/install.sh" > "$work/install.sh"
sh "$here/sign-release-assets.sh" "$work/release/$version" "$version" "$work/key" "$work/install.sh" > /dev/null
cp -R "$work/release" "$work/tampered"
printf 'tamper\n' >> "$work/tampered/$version/pb-linux-amd64"; printf 'tamper\n' >> "$work/tampered/$version/pb-linux-arm64"
cat > "$work/bin/curl" <<'CURL'
#!/bin/sh
out= url=
while [ "$#" -gt 0 ]; do
  case "$1" in -o) out=$2; shift 2 ;; -w|--connect-timeout|--max-time|--max-filesize|--proto|--proto-redir) shift 2 ;; -*) shift ;; *) url=$1; shift ;; esac
done
case "$url" in
  https://github.com/example/paperboat/releases/latest) printf '%s' "https://github.com/example/paperboat/releases/tag/$FAKE_VERSION" ;;
  https://github.com/example/paperboat/releases/download/*/*)
    file=${url##*/}; v=${url%/*}; v=${v##*/}
    [ -f "/release/$v/$file" ] || exit 22
    cp "/release/$v/$file" "$out" ;;
  *) exit 22 ;;
esac
CURL
chmod 0755 "$work/bin/curl" "$work/release/$version"/pb-linux-*

for image in "${images[@]}"; do
  echo "== $image"
  run() { # run RELEASE_DIR SCRIPT: the installer as root inside the image
    $docker run --rm -v "$work/$1:/release:ro" -v "$work/bin/curl:/usr/local/bin/curl:ro" -v "$work/install.sh:/install.sh:ro" \
      -e FAKE_VERSION="$version" -e PB_REPO=example/paperboat "$image" sh -c "$2"
  }
  # The installer needs python3 to read pb's JSON (not what is under test): give it one where the image has none.
  prepare='command -v ssh-keygen >/dev/null 2>&1 && { echo "image already has ssh-keygen"; exit 90; }
    command -v python3 >/dev/null 2>&1 || { if command -v apk >/dev/null 2>&1; then apk add --no-cache python3 >/dev/null; elif command -v dnf >/dev/null 2>&1; then dnf install -y python3 >/dev/null 2>&1; fi; }'
  out=$(run release "$prepare; sh /install.sh --no-setup --install-dir /opt/pb 2>&1; echo rc=\$?; command -v ssh-keygen; cat /tmp/pb-calls || true") || fail "$image: container run failed: $out"
  echo "$out" | grep -q 'installing OpenSSH to verify the release signature' || fail "$image: the installer did not install OpenSSH first: $out"
  echo "$out" | grep -q '^rc=0$' || fail "$image: install did not succeed: $out"
  echo "$out" | grep -q '^/usr/bin/ssh-keygen$\|^/usr/sbin/ssh-keygen$\|/ssh-keygen$' || fail "$image: ssh-keygen is not installed afterwards: $out"
  echo "$out" | grep -q 'install --install-dir /opt/pb --json' || fail "$image: pb install was not run: $out"
  out=$(run tampered "$prepare; sh /install.sh --no-setup --install-dir /opt/pb 2>&1; echo rc=\$?; ls /tmp/pb-calls 2>&1 || true") || fail "$image: container run failed: $out"
  echo "$out" | grep -q 'product digest mismatch' || fail "$image: a tampered product was not refused: $out"
  echo "$out" | grep -q 'No such file' || fail "$image: pb ran although the product was tampered: $out"
  echo "ok $image"
done
echo "container installs: ok"
