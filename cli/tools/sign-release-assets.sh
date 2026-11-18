#!/bin/sh
# Writes SHA256SUMS and SHA256SUMS.sig for every file in DIRECTORY, signed with the release SSH key, then proves each
# given installer's embedded public key verifies the signature (a release that no installer can verify is refused).
# usage: sign-release-assets.sh DIRECTORY TAG KEY_FILE INSTALLER...
set -eu
[ "$#" -ge 4 ] || { echo "usage: sign-release-assets.sh DIRECTORY TAG KEY_FILE INSTALLER..." >&2; exit 2; }
directory=$1 tag=$2 key=$3
shift 3
[ -d "$directory" ] && [ -f "$key" ] || { echo "sign-release-assets: directory or key is missing" >&2; exit 2; }
work=$(mktemp -d)
for installer in "$@"; do [ -f "$installer" ] || { echo "sign-release-assets: $installer is missing" >&2; exit 2; }; done
for installer in "$@"; do set -- "$@" "$(cd "$(dirname "$installer")" && pwd)/$(basename "$installer")"; shift; done
trap 'rm -rf -- "$work"' EXIT
cd "$directory"
rm -f SHA256SUMS SHA256SUMS.sig
{
  # The first line names the release: a signed SHA256SUMS from another release cannot be replayed under this tag.
  printf '# paperboat-release %s\n' "$tag"
  for file in *; do [ -f "$file" ] && sha256sum "$file"; done
} > "$work/SHA256SUMS"
cp "$work/SHA256SUMS" SHA256SUMS
ssh-keygen -q -Y sign -f "$key" -n paperboat-release SHA256SUMS
for installer in "$@"; do
  line=$(grep -o 'paperboat-release namespaces="paperboat-release" ssh-ed25519 [A-Za-z0-9+/=]*' "$installer" | head -n 1)
  [ -n "$line" ] || { echo "sign-release-assets: $installer embeds no release key" >&2; exit 1; }
  printf '%s\n' "$line" > "$work/allowed_signers"
  ssh-keygen -Y verify -f "$work/allowed_signers" -I paperboat-release -n paperboat-release -s SHA256SUMS.sig < SHA256SUMS > /dev/null || { echo "sign-release-assets: the key embedded in $installer does not verify this release" >&2; exit 1; }
done
echo "signed $(grep -c . SHA256SUMS) lines of SHA256SUMS for $tag"
