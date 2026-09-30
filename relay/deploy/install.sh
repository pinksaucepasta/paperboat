#!/bin/sh
# Served at https://get.pprbt.dev/selfhost/install with release values filled in.
set -eu
case "$(uname -s)" in Linux) ;; *) echo 'Paperboat self-host currently requires Linux with systemd.' >&2; exit 1;; esac
case "$(uname -m)" in
 x86_64) package_arch=amd64; package_url='{{PACKAGE_URL_AMD64}}'; package_sha256='{{PACKAGE_SHA256_AMD64}}';;
 aarch64|arm64) package_arch=arm64; package_url='{{PACKAGE_URL_ARM64}}'; package_sha256='{{PACKAGE_SHA256_ARM64}}';;
 *) echo 'Unsupported self-host architecture.' >&2; exit 1;;
esac
case "$package_url" in https://*) ;; *) echo 'Self-host release is not configured.' >&2; exit 1;; esac
if [ ${#package_sha256} -ne 64 ]; then echo 'Self-host release digest is not configured.' >&2; exit 1; fi
case "$package_sha256" in *[!0-9a-f]*) echo 'Invalid release digest.' >&2; exit 1;; esac
for tool in curl tar sha256sum install systemctl; do command -v "$tool" >/dev/null 2>&1 || { echo "Required tool unavailable: $tool" >&2; exit 1; }; done
if [ "$(id -u)" -eq 0 ]; then elevate=''; else command -v sudo >/dev/null 2>&1 || { echo 'sudo is required to install the system service.' >&2; exit 1; }; elevate=sudo; $elevate -v; fi
install_tmp=$(mktemp -d)
previous_active=false
mutation_started=false
installation_complete=false
cleanup() {
 result=$?
 trap - EXIT HUP INT TERM
 if [ "$mutation_started" = true ] && [ "$installation_complete" = false ]; then
  echo 'Installation failed; restoring previous executables and service.' >&2
  for binary in pbh paperboat-relay paperboat-tunnel; do
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
curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 "$package_url" -o "$install_tmp/package.tar.gz"
printf '%s  %s\n' "$package_sha256" "$install_tmp/package.tar.gz" | sha256sum --check --status || { echo 'Release integrity check failed; installation unchanged.' >&2; exit 1; }
# Accept only the exact package members, even though the digest binds the archive.
tar -tzf "$install_tmp/package.tar.gz" > "$install_tmp/members"
while IFS= read -r member; do case "$member" in pbh|paperboat-relay|paperboat-tunnel|LICENSE|tailscale.LICENSE|manifest.json) ;; *) echo 'Invalid package member; installation unchanged.' >&2; exit 1;; esac; done < "$install_tmp/members"
tar -xzf "$install_tmp/package.tar.gz" --no-same-owner --no-same-permissions -C "$install_tmp"
for binary in pbh paperboat-relay paperboat-tunnel; do [ -f "$install_tmp/$binary" ] && [ ! -L "$install_tmp/$binary" ] || { echo 'Incomplete release package; installation unchanged.' >&2; exit 1; }; done
# Preserve executable provenance before changing an existing installation.
for binary in pbh paperboat-relay paperboat-tunnel; do
 if [ -f "/usr/local/lib/paperboat-selfhost/$binary" ]; then cp "/usr/local/lib/paperboat-selfhost/$binary" "$install_tmp/previous-$binary"; fi
done
if [ -f /usr/local/bin/pbh ]; then cp /usr/local/bin/pbh "$install_tmp/previous-command"; fi
if [ -f /etc/systemd/system/paperboat-selfhost.service ]; then cp /etc/systemd/system/paperboat-selfhost.service "$install_tmp/previous-unit"; fi
# Stop an existing installation before atomically replacing executable files.
if systemctl is-active --quiet paperboat-selfhost.service; then previous_active=true; $elevate systemctl stop paperboat-selfhost.service; fi
mutation_started=true
$elevate install -d -m 0755 /usr/local/lib/paperboat-selfhost
for binary in pbh paperboat-relay paperboat-tunnel; do
 $elevate install -m 0755 "$install_tmp/$binary" "/usr/local/lib/paperboat-selfhost/$binary.new"
 $elevate mv "/usr/local/lib/paperboat-selfhost/$binary.new" "/usr/local/lib/paperboat-selfhost/$binary"
done
$elevate install -m 0755 "$install_tmp/pbh" /usr/local/bin/pbh
$elevate /usr/local/lib/paperboat-selfhost/pbh install "$@"

installation_complete=true
