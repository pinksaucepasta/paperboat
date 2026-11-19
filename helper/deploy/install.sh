#!/bin/sh
# The fixed Paperboat self-host installer: one file for every release. It finds the newest `selfhost-<version>` GitHub
# release, downloads the package for this CPU with that release's SHA256SUMS and SHA256SUMS.sig, verifies the signature
# with the key embedded below, and only then checks the package hash. No Paperboat server is involved.
# Environment (optional): PB_VERSION installs that self-host version, PB_REPO is the owner/repo that publishes releases.
set -eu
repository=${PB_REPO:-pinksaucepasta/paperboat}
# The release signing key (ssh-keygen -Y verify allowed_signers line); the private half is only in CI.
allowed_signers='paperboat-release namespaces="paperboat-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAonuBw7PaD+214NxAVb1UwkLzJYWq0VRmpbM2EXsBhg'
case $repository in */*) ;; *) echo 'PB_REPO must look like owner/repo.' >&2; exit 2;; esac
case /$repository/ in *//*|*/./*|*/../*|*[!A-Za-z0-9_./-]*) echo 'PB_REPO must look like owner/repo.' >&2; exit 2;; esac
case $repository in */*/*) echo 'PB_REPO must look like owner/repo.' >&2; exit 2;; esac
case "$(uname -s)" in Linux) ;; *) echo 'Paperboat self-host currently requires Linux with systemd.' >&2; exit 1;; esac
case "$(uname -m)" in
 x86_64) package_arch=amd64;;
 aarch64|arm64) package_arch=arm64;;
 *) echo 'Unsupported self-host architecture.' >&2; exit 1;;
esac
# A missing download tool is an error that says what to install (Tailscale scripts/installer.sh 399-407).
command -v curl >/dev/null 2>&1 || { echo 'The installer needs curl to download files.' >&2; echo 'Please install curl to proceed.' >&2; exit 1; }
for tool in tar sha256sum install systemctl timeout; do command -v "$tool" >/dev/null 2>&1 || { echo "Required tool unavailable: $tool" >&2; exit 1; }; done
# Choose what to run before anything is downloaded or changed. Stdin is the pipe from curl, so the
# questions go to the terminal, as rustup-init.sh does. Nothing else is required: the host's public
# address is detected and the claim code is printed at the end.
install_url=https://get.pprbt.dev/selfhost
components=0
for arg in "$@"; do
 case "$arg" in
  --relay|--edge|--both) components=$((components + 1));;
  --enable|--enable=*) echo 'The --enable flag no longer exists; use --relay, --edge or --both.' >&2; exit 2;;
 esac
done
if [ "$components" -gt 1 ]; then echo 'Choose only one of --relay, --edge and --both.' >&2; exit 2; fi
# An existing installation keeps its components, so upgrades ask nothing.
if [ "$components" -eq 0 ] && ! systemctl cat paperboat-selfhost.service >/dev/null 2>&1; then
 if ! (: </dev/tty) 2>/dev/null; then
  echo 'Choose what to run on this host. Run the installer again with one of these flags:' >&2
  for flag in relay edge both; do echo "  curl -fsSL $install_url | sh -s -- --$flag" >&2; done
  exit 2
 fi
 printf 'What should this host run?\n  1) Relay - carries your machines'"'"' connections when they cannot connect directly\n  2) Edge  - serves your previews and tunnels\n  3) Both\n' >&2
 printf 'Choose 1-3 [3]: ' >&2
 read -r answer </dev/tty || answer=''
 case "$answer" in 1) choice=relay; what='a relay';; 2) choice=edge; what='an edge';; 3|'') choice=both; what='a relay and an edge';; *) echo 'Invalid choice; installation unchanged.' >&2; exit 2;; esac
 printf 'Install %s on this host? [Y/n] ' "$what" >&2
 read -r answer </dev/tty || answer=''
 case "$answer" in ''|y|Y|yes|YES) ;; *) echo 'Installation cancelled; nothing changed.' >&2; exit 1;; esac
 set -- "--$choice" "$@"
fi
# Root helper found like NetBird's installer (release_files/install.sh 25-30): sudo, else doas, unless already root.
elevate=
if [ "$(id -u)" -ne 0 ]; then
 if command -v sudo >/dev/null 2>&1; then elevate=sudo; $elevate -v
 elif command -v doas >/dev/null 2>&1; then elevate=doas
 else echo 'sudo or doas is required to install the system service.' >&2; exit 1; fi
fi
install_tmp=$(mktemp -d)
previous_active=false
mutation_started=false
installation_complete=false
cleanup() {
 result=$?
 trap - EXIT HUP INT TERM
 if [ "$mutation_started" = true ] && [ "$installation_complete" = false ]; then
  echo 'Installation failed; restoring previous executable, image configuration and service.' >&2
  $elevate timeout 30 systemctl stop paperboat-selfhost.service >/dev/null 2>&1 || true
  if [ -f "$install_tmp/previous-images" ]; then $elevate install -m 0600 "$install_tmp/previous-images" /var/lib/paperboat-selfhost/images.json || true; fi
  for binary in pbh; do
   if [ -f "$install_tmp/previous-$binary" ]; then
    $elevate install -m 0755 "$install_tmp/previous-$binary" "/usr/local/lib/paperboat-selfhost/$binary" || true
   else
    $elevate rm -f "/usr/local/lib/paperboat-selfhost/$binary" || true
   fi
   $elevate rm -f "/usr/local/lib/paperboat-selfhost/$binary.new" || true
  done
  if [ -f "$install_tmp/previous-command" ]; then $elevate install -m 0755 "$install_tmp/previous-command" /usr/local/bin/pbh || true; else $elevate rm -f /usr/local/bin/pbh || true; fi
  if [ -f "$install_tmp/previous-unit" ]; then
   $elevate install -m 0644 "$install_tmp/previous-unit" /etc/systemd/system/paperboat-selfhost.service || true
  else
   $elevate systemctl disable --now paperboat-selfhost.service >/dev/null 2>&1 || true
   $elevate rm -f /etc/systemd/system/paperboat-selfhost.service || true
  fi
  $elevate systemctl daemon-reload || true
  if [ "$previous_active" = true ]; then $elevate systemctl restart paperboat-selfhost.service || echo 'Previous files restored; restart paperboat-selfhost.service manually.' >&2; fi
 fi
 rm -rf "$install_tmp"
 exit "$result"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM
fetch() { curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 --proto-redir '=https' --connect-timeout 15 --max-time "$2" --max-filesize "$3" "$1" -o "$4"; }
# The newest self-host release is the highest `selfhost-<version>` tag; these releases are never marked Latest.
version=${PB_VERSION:-}
if [ -z "$version" ]; then
 curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 --proto-redir '=https' --connect-timeout 15 --max-time 60 "https://api.github.com/repos/$repository/git/matching-refs/tags/selfhost-" -o "$install_tmp/tags" || { echo 'Could not reach GitHub to find the latest self-host release; check your network and rerun.' >&2; exit 1; }
 version=$(grep -o 'refs/tags/selfhost-[0-9][0-9.]*' "$install_tmp/tags" | sed 's#refs/tags/selfhost-##' | sort -t. -k1,1n -k2,2n -k3,3n -k4,4n | tail -n 1)
 [ -n "$version" ] || { echo "$repository has no published self-host release yet." >&2; exit 1; }
fi
case $version in 20[0-9][0-9].[0-9][0-9].[0-9][0-9].*) ;; *) echo "$version is not a release version like 2026.11.18.1." >&2; exit 1;; esac
case ${version#????.??.??.} in 0) ;; ''|0*|*[!0123456789]*) echo "$version is not a release version like 2026.11.18.1." >&2; exit 1;; esac
tag=selfhost-$version
release_url="https://github.com/$repository/releases/download/$tag"
fetch "$release_url/SHA256SUMS" 60 1048576 "$install_tmp/SHA256SUMS" || { echo "Could not download SHA256SUMS for $tag; check the version and your network." >&2; exit 1; }
fetch "$release_url/SHA256SUMS.sig" 60 65536 "$install_tmp/SHA256SUMS.sig" || { echo "Could not download SHA256SUMS.sig for $tag." >&2; exit 1; }
# A missing ssh-keygen is installed with the system package manager, as NetBird and Tailscale install gnupg
# (NetBird add_apt_repo, Tailscale scripts/installer.sh 545-548); verification is never skipped.
if ! command -v ssh-keygen >/dev/null 2>&1; then
 echo 'ssh-keygen is missing; installing OpenSSH to verify the release signature.' >&2
 if command -v apt-get >/dev/null 2>&1; then $elevate apt-get update -qq && $elevate env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq openssh-client
 elif command -v dnf >/dev/null 2>&1; then $elevate dnf install -y openssh
 elif command -v yum >/dev/null 2>&1; then $elevate yum install -y openssh
 elif command -v apk >/dev/null 2>&1; then $elevate apk add --no-cache openssh-keygen
 elif command -v pacman >/dev/null 2>&1; then $elevate pacman -S --noconfirm openssh
 elif command -v zypper >/dev/null 2>&1; then $elevate zypper --non-interactive install openssh
 else false; fi || { echo 'ssh-keygen is required to verify the release signature and could not be installed here; install OpenSSH (openssh-client) and rerun.' >&2; exit 1; }
 command -v ssh-keygen >/dev/null 2>&1 || { echo 'ssh-keygen is still missing after installing OpenSSH.' >&2; exit 1; }
fi
printf '%s\n' "$allowed_signers" > "$install_tmp/allowed_signers"
ssh-keygen -Y verify -f "$install_tmp/allowed_signers" -I paperboat-release -n paperboat-release -s "$install_tmp/SHA256SUMS.sig" < "$install_tmp/SHA256SUMS" > /dev/null 2>&1 || { echo 'The release signature on SHA256SUMS is not valid (needs ssh-keygen from OpenSSH 8.0 or newer); nothing was installed.' >&2; exit 1; }
[ "$(sed -n 1p "$install_tmp/SHA256SUMS")" = "# paperboat-release $tag" ] || { echo "SHA256SUMS is signed for a different release than $tag." >&2; exit 1; }
package=paperboat-selfhost-linux-$package_arch.tar.gz
package_sha256=$(awk -v a="$package" '$2 == a { print $1; n++ } END { if (n != 1) exit 1 }' "$install_tmp/SHA256SUMS") || { echo "SHA256SUMS does not list $package exactly once." >&2; exit 1; }
if [ ${#package_sha256} -ne 64 ]; then echo 'Invalid release digest.' >&2; exit 1; fi
case "$package_sha256" in *[!0-9a-f]*) echo 'Invalid release digest.' >&2; exit 1;; esac
fetch "$release_url/$package" 120 134217728 "$install_tmp/package.tar.gz"
printf '%s  %s\n' "$package_sha256" "$install_tmp/package.tar.gz" | sha256sum --check --status || { echo 'Release integrity check failed; installation unchanged.' >&2; exit 1; }
# Accept only the exact package members, even though the digest binds the archive.
tar -tzf "$install_tmp/package.tar.gz" > "$install_tmp/members"
while IFS= read -r member; do case "$member" in pbh|LICENSE|tailscale.LICENSE|manifest.json) ;; *) echo 'Invalid package member; installation unchanged.' >&2; exit 1;; esac; done < "$install_tmp/members"
tar -xzf "$install_tmp/package.tar.gz" --no-same-owner --no-same-permissions -C "$install_tmp"
for binary in pbh; do [ -f "$install_tmp/$binary" ] && [ ! -L "$install_tmp/$binary" ] || { echo 'Incomplete release package; installation unchanged.' >&2; exit 1; }; done
# The helper owns the supported-host policy; refuse before privileged changes.
"$install_tmp/pbh" check-host
# Docker Engine, the documented production way (docs.docker.com/engine/install): Docker's own apt repository, pinned
# to its signing key's published fingerprint. No downloaded script is run. An installed Docker that is new enough is
# left alone (no Docker daemon configuration is changed either way).
docker_min_major=20 docker_min_minor=10
docker_key_fingerprint=9DC858229FC7DD38854AE2D88D81803C0EBFCD88
if command -v docker >/dev/null 2>&1; then
 docker_version=$(docker --version 2>/dev/null | sed -n 's/^Docker version \([0-9][0-9]*\)\.\([0-9][0-9]*\).*/\1 \2/p')
 docker_major=${docker_version%% *} docker_minor=${docker_version#* }
 case "$docker_major$docker_minor" in ''|*[!0-9]*) echo 'Could not read the installed Docker version; install Docker Engine 20.10 or newer (docs.docker.com/engine/install) and rerun.' >&2; exit 1;; esac
 if [ "$docker_major" -lt "$docker_min_major" ] || { [ "$docker_major" -eq "$docker_min_major" ] && [ "$docker_minor" -lt "$docker_min_minor" ]; }; then
  echo "Docker $docker_major.$docker_minor is older than $docker_min_major.$docker_min_minor; upgrade Docker Engine (docs.docker.com/engine/install) and rerun." >&2; exit 1
 fi
else
 command -v apt-get >/dev/null 2>&1 || { echo 'Docker is required and could not be installed here; install Docker Engine (docs.docker.com/engine/install) and rerun.' >&2; exit 1; }
 docker_os=$(. /etc/os-release && echo "${ID:-}") docker_codename=$(. /etc/os-release && echo "${UBUNTU_CODENAME:-${VERSION_CODENAME:-}}")
 case $docker_os in ubuntu|debian) ;; *) if [ -n "$(. /etc/os-release && echo "${UBUNTU_CODENAME:-}")" ]; then docker_os=ubuntu; else echo "Docker's repository does not cover $docker_os; install Docker Engine (docs.docker.com/engine/install) and rerun." >&2; exit 1; fi;; esac
 case $docker_codename in ''|*[!a-z]*) echo 'Could not read this system'"'"'s release name; install Docker Engine (docs.docker.com/engine/install) and rerun.' >&2; exit 1;; esac
 echo 'Installing Docker Engine from Docker'"'"'s official apt repository.'
 $elevate apt-get update -qq && $elevate env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ca-certificates curl gnupg
 curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 --proto-redir '=https' --connect-timeout 10 --max-time 60 --max-filesize 1048576 "https://download.docker.com/linux/$docker_os/gpg" -o "$install_tmp/docker.asc"
 # The key is pinned to the fingerprint Docker publishes: exactly one primary key, and it is that one.
 key_fingerprints=$(gpg --show-keys --with-colons "$install_tmp/docker.asc" | awk -F: '$1 == "pub" { want = 1; next } $1 == "fpr" && want { print $10; want = 0 }')
 [ "$key_fingerprints" = "$docker_key_fingerprint" ] || { echo "Docker's signing key does not match its published fingerprint; nothing was installed." >&2; exit 1; }
 $elevate install -m 0755 -d /etc/apt/keyrings
 $elevate install -m 0644 "$install_tmp/docker.asc" /etc/apt/keyrings/docker.asc
 printf 'deb [arch=%s signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/%s %s stable\n' "$(dpkg --print-architecture)" "$docker_os" "$docker_codename" > "$install_tmp/docker.list"
 $elevate install -m 0644 "$install_tmp/docker.list" /etc/apt/sources.list.d/docker.list
 $elevate apt-get update -qq && $elevate env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq docker-ce docker-ce-cli containerd.io
fi
$elevate docker info >/dev/null || { echo 'Docker daemon is unavailable; start Docker and rerun.' >&2; exit 1; }
# Preserve executable provenance before changing an existing installation.
for binary in pbh; do
 if [ -f "/usr/local/lib/paperboat-selfhost/$binary" ]; then cp "/usr/local/lib/paperboat-selfhost/$binary" "$install_tmp/previous-$binary"; fi
done
if [ -f /usr/local/bin/pbh ]; then cp /usr/local/bin/pbh "$install_tmp/previous-command"; fi
if [ -f /var/lib/paperboat-selfhost/images.json ]; then $elevate cp /var/lib/paperboat-selfhost/images.json "$install_tmp/previous-images"; fi
if [ -f /etc/systemd/system/paperboat-selfhost.service ]; then cp /etc/systemd/system/paperboat-selfhost.service "$install_tmp/previous-unit"; fi
# Stop an existing installation before atomically replacing executable files.
mutation_started=true
if systemctl is-active --quiet paperboat-selfhost.service; then previous_active=true; $elevate timeout 30 systemctl stop paperboat-selfhost.service; fi
$elevate install -d -m 0755 /usr/local/lib/paperboat-selfhost
for binary in pbh; do
 $elevate install -m 0755 "$install_tmp/$binary" "/usr/local/lib/paperboat-selfhost/$binary.new"
 $elevate mv "/usr/local/lib/paperboat-selfhost/$binary.new" "/usr/local/lib/paperboat-selfhost/$binary"
done
$elevate install -m 0755 "$install_tmp/pbh" /usr/local/bin/pbh
$elevate /usr/local/lib/paperboat-selfhost/pbh install "$@"

installation_complete=true
# A successful Docker transition no longer needs the retired host runtimes.
for binary in paperboat-relay paperboat-edge; do
 $elevate rm -f "/usr/local/lib/paperboat-selfhost/$binary"
done
