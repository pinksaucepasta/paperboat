#!/bin/sh
# The fixed Paperboat installer: one file for every release. It finds the latest GitHub release, downloads
# the binary with the release's SHA256SUMS and SHA256SUMS.sig, verifies the signature with the key embedded
# below, and only then trusts the hashes. No Paperboat server is involved. Environment (all optional):
#   PB_TOKEN    machine enrollment token from the console (Add machine)
#   PB_RECIPE   recipe token: pair this computer from a recipe instead
#   PB_NAME     machine name        PB_VERSION  install this release instead of the latest
#   PB_REPO     owner/repo that publishes the releases
# Keep a truncated curl | sh download from running a partial installation.
main() {
set -eu

repository=${PB_REPO:-pinksaucepasta/paperboat}
# The release signing key (ssh-keygen -Y verify allowed_signers line); the private half is only in CI.
allowed_signers='paperboat-release namespaces="paperboat-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAonuBw7PaD+214NxAVb1UwkLzJYWq0VRmpbM2EXsBhg'
requested_version=${PB_VERSION:-latest}
install_dir=${PAPERBOAT_INSTALL_DIR:-"${HOME}/.local/bin"}
install_dir_requested=false
setup=false
pair=false
join=true
token=${PB_TOKEN:-}
token_file=
machine_name=${PB_NAME:-}
ssh_port=
recovery_output=
unset PB_TOKEN || true
[ -z "$token" ] || pair=true
case $repository in */*) ;; *) echo "pb installer: PB_REPO must look like owner/repo" >&2; exit 2 ;; esac
case /$repository/ in *//*|*/./*|*/../*|*[!A-Za-z0-9_./-]*) echo "pb installer: PB_REPO must look like owner/repo" >&2; exit 2 ;; esac
case $repository in */*/*) echo "pb installer: PB_REPO must look like owner/repo" >&2; exit 2 ;; esac
# PB_RECIPE pairs this computer from a recipe token. It refuses at once, before anything is installed, when
# the value is not a recipe token (PLAN F26).
recipe=false
if [ -n "${PB_RECIPE:-}" ]; then
  recipe=true join=false
  case $PB_RECIPE in
    pbr_*) [ "${#PB_RECIPE}" -eq 67 ] || recipe_bad=true ;;
    *) recipe_bad=true ;;
  esac
  if [ "${recipe_bad:-false}" = true ]; then
    echo "Paperboat: PB_RECIPE is not a recipe token. A token starts with pbr_ and is 67 characters; this value is ${#PB_RECIPE}. Copy it again from where you saved it." >&2
    exit 2
  fi
fi

usage() { cat <<'EOF'
Install the current Paperboat release.

Usage: install.sh [options]
  --version VERSION             Install this release instead of the latest
  --install-dir DIRECTORY       Install Linux pb here (default: ~/.local/bin)
  --setup                       Run pb setup after installation
  --pair                        Run pb pair after installation
  --enrollment-token TOKEN      Use a dashboard-issued pairing token
  --enrollment-token-file FILE  Read a pairing token from an absolute file
  --name NAME                   Set the machine name during setup or pairing
  --ssh-port PORT               Existing SSH port used by managed SSH
  --recovery-output FILE        Save the recovery key during setup
  --no-setup                    Install only; do not add this computer to your account
  -h, --help                    Show this help
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version|--install-dir|--enrollment-token|--enrollment-token-file|--name|--ssh-port|--recovery-output)
      [ "$#" -ge 2 ] || { echo "pb installer: $1 requires a value" >&2; exit 2; }
      case "$1" in
        --version) requested_version=$2 ;;
        --install-dir) install_dir=$2; install_dir_requested=true ;;
        --enrollment-token) token=$2; pair=true ;;
        --enrollment-token-file) token_file=$2; pair=true ;;
        --name) machine_name=$2 ;;
        --ssh-port) ssh_port=$2 ;;
        --recovery-output) recovery_output=$2 ;;
      esac
      shift 2 ;;
    --setup) setup=true; shift ;;
    --pair) pair=true; shift ;;
    --no-setup) setup=false; pair=false; join=false; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "pb installer: unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[ "$setup" != true ] || [ "$pair" != true ] || { echo "pb installer: use either --setup or --pair" >&2; exit 2; }
[ "$recipe" != true ] || { [ "$setup" != true ] && [ "$pair" != true ]; } || { echo "pb installer: a recipe install pairs from PB_RECIPE; drop --setup, --pair and enrollment tokens" >&2; exit 2; }
if [ "$setup" = true ] || [ "$pair" = true ]; then join=false; fi
[ -z "$token" ] || [ -z "$token_file" ] || { echo "pb installer: use only one enrollment token source" >&2; exit 2; }
[ "$pair" != true ] || [ -n "$token" ] || [ -n "$token_file" ] || { echo "pb installer: --pair requires --enrollment-token or --enrollment-token-file" >&2; exit 2; }
[ -z "$ssh_port" ] || [ "$setup" = true ] || [ "$pair" = true ] || { echo "pb installer: --ssh-port requires --setup or --pair" >&2; exit 2; }
[ -z "$recovery_output" ] || [ "$setup" = true ] || { echo "pb installer: --recovery-output requires --setup" >&2; exit 2; }

case $(uname -s) in Darwin) os=darwin ;; Linux) os=linux ;; *) echo "pb installer: only macOS and Linux are supported" >&2; exit 1 ;; esac
case $(uname -m) in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) echo "pb installer: unsupported architecture: $(uname -m)" >&2; exit 1 ;; esac
[ "$os" != darwin ] || [ "$arch" = arm64 ] || { echo "pb installer: macOS releases support arm64" >&2; exit 1; }

# Tailscale's installer (scripts/installer.sh 399-407) stops the same way when its download tool is missing.
command -v curl >/dev/null 2>&1 || { echo "The installer needs curl to download files." >&2; echo "Please install curl to proceed." >&2; exit 1; }

# TMPDIR may end in "/" (macOS) or repeat slashes: pb rejects a token file path that is not clean, so use the resolved directory.
tmp_root=${TMPDIR:-/tmp}
temporary=$(mktemp -d "${tmp_root%/}/paperboat-install.XXXXXX")
temporary=$(cd "$temporary" && pwd -P)
# macOS sh keeps a leading "//" (POSIX leaves it implementation-defined) when TMPDIR starts with two slashes.
temporary=/${temporary#"${temporary%%[!/]*}"}
cleanup() { rm -rf -- "$temporary"; }
trap cleanup EXIT HUP INT TERM
if [ "$pair" = true ]; then
  staged_token_file="$temporary/enrollment-token"
  if [ -n "$token_file" ]; then
    case "$token_file" in /*) ;; *) echo "pb installer: --enrollment-token-file must be absolute" >&2; exit 2 ;; esac
    [ -f "$token_file" ] && [ ! -L "$token_file" ] && [ -O "$token_file" ] || { echo "pb installer: enrollment token file must be a regular owner-owned file" >&2; exit 2; }
    if stat -f '%Lp' "$token_file" >/dev/null 2>&1; then token_mode=$(stat -f '%Lp' "$token_file"); else token_mode=$(stat -c '%a' "$token_file"); fi
    case "$token_mode" in *00) ;; *) echo "pb installer: enrollment token file must not grant group or other access" >&2; exit 2 ;; esac
    token_size=$(wc -c < "$token_file" | tr -d ' ')
    [ "$token_size" -le 512 ] || { echo "pb installer: enrollment token file is too large" >&2; exit 2; }
    token=$(cat "$token_file")
  fi
  [ -n "$token" ] && [ "${#token}" -le 512 ] || { echo "pb installer: enrollment token is invalid" >&2; exit 2; }
  (umask 077 && printf '%s' "$token" > "$staged_token_file")
  token_file=$staged_token_file
  token=
fi
asset="pb-${os}-${arch}"
# Verifying the release signature needs ssh-keygen (always present on macOS). A missing one is installed with the
# system package manager, as NetBird's and Tailscale's installers install gnupg: the package manager is found with
# `command -v` in NetBird's order (release_files/install.sh, apt-get, dnf, yum, ...) and the root helper the same
# way NetBird finds it (sudo, else doas, unless already root; release_files/install.sh lines 25-30).
SUDO=
if command -v sudo >/dev/null 2>&1 && [ "$(id -u)" -ne 0 ]; then SUDO=sudo
elif command -v doas >/dev/null 2>&1 && [ "$(id -u)" -ne 0 ]; then SUDO=doas; fi
install_ssh_keygen() {
  [ "$(id -u)" -eq 0 ] || [ -n "$SUDO" ] || return 1
  echo "pb installer: ssh-keygen is missing; installing OpenSSH to verify the release signature" >&2
  if command -v apt-get >/dev/null 2>&1; then
    # Tailscale (scripts/installer.sh 545-548) and NetBird (add_apt_repo) both run apt-get update before the install.
    $SUDO apt-get update -qq && $SUDO env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq openssh-client
  elif command -v dnf >/dev/null 2>&1; then $SUDO dnf install -y openssh
  elif command -v yum >/dev/null 2>&1; then $SUDO yum install -y openssh
  elif command -v apk >/dev/null 2>&1; then $SUDO apk add --no-cache openssh-keygen
  elif command -v pacman >/dev/null 2>&1; then $SUDO pacman -S --noconfirm openssh
  elif command -v zypper >/dev/null 2>&1; then $SUDO zypper --non-interactive install openssh
  else return 1; fi
}
download() { curl --fail --location --show-error --silent --connect-timeout 15 --max-time "$2" --max-filesize "$3" --proto '=https' --proto-redir '=https' "$1" -o "$4"; }
# The newest release is the one GitHub marks Latest (set only after the release is fully published).
if [ "$requested_version" = latest ]; then
  latest_url=$(curl --silent --show-error --location --head --connect-timeout 15 --max-time 60 --proto '=https' --proto-redir '=https' -o /dev/null -w '%{url_effective}' "https://github.com/$repository/releases/latest") || { echo "pb installer: could not reach GitHub to find the latest release; check your network and rerun" >&2; exit 1; }
  case $latest_url in */releases/tag/*) version=${latest_url##*/} ;; *) echo "pb installer: $repository has no published release yet" >&2; exit 1 ;; esac
else
  version=$requested_version
fi
case $version in 20[0-9][0-9].[0-9][0-9].[0-9][0-9].*) ;; *) echo "pb installer: $version is not a release version like 2026.11.18.1" >&2; exit 1 ;; esac
case ${version#????.??.??.} in 0) ;; ''|0*|*[!0123456789]*) echo "pb installer: $version is not a release version like 2026.11.18.1" >&2; exit 1 ;; esac
release_url="https://github.com/$repository/releases/download/$version"
# SHA256SUMS carries the release signature only when its first line names this version, so a signed file from
# another release cannot be replayed here.
download "$release_url/SHA256SUMS" 60 1048576 "$temporary/SHA256SUMS" || { echo "pb installer: could not download SHA256SUMS for $version; check the version and your network" >&2; exit 1; }
download "$release_url/SHA256SUMS.sig" 60 65536 "$temporary/SHA256SUMS.sig" || { echo "pb installer: could not download SHA256SUMS.sig for $version" >&2; exit 1; }
command -v ssh-keygen >/dev/null 2>&1 || install_ssh_keygen || { echo "pb installer: ssh-keygen is required to verify the release signature and could not be installed here; install OpenSSH (openssh-client) and rerun" >&2; exit 1; }
command -v ssh-keygen >/dev/null 2>&1 || { echo "pb installer: ssh-keygen is still missing after installing OpenSSH" >&2; exit 1; }
printf '%s\n' "$allowed_signers" > "$temporary/allowed_signers"
ssh-keygen -Y verify -f "$temporary/allowed_signers" -I paperboat-release -n paperboat-release -s "$temporary/SHA256SUMS.sig" < "$temporary/SHA256SUMS" > /dev/null 2>&1 || { echo "pb installer: the release signature on SHA256SUMS is not valid (needs ssh-keygen from OpenSSH 8.0 or newer); nothing was installed" >&2; exit 1; }
[ "$(sed -n 1p "$temporary/SHA256SUMS")" = "# paperboat-release $version" ] || { echo "pb installer: SHA256SUMS is signed for a different release than $version" >&2; exit 1; }
product_sha=$(awk -v a="$asset" '$2 == a { print $1; n++ } END { if (n != 1) exit 1 }' "$temporary/SHA256SUMS") || { echo "pb installer: SHA256SUMS does not list $asset exactly once" >&2; exit 1; }
[ "${#product_sha}" -eq 64 ] || { echo 'pb installer: published product digest is invalid' >&2; exit 1; }
case "$product_sha" in *[!0123456789abcdef]*) echo 'pb installer: published product digest is invalid' >&2; exit 1 ;; esac
product_file="$temporary/$asset"
download "$release_url/$asset" 300 536870912 "$product_file"
if command -v shasum >/dev/null 2>&1; then actual_sha=$(shasum -a 256 "$product_file" | awk '{print $1}');
elif command -v sha256sum >/dev/null 2>&1; then actual_sha=$(sha256sum "$product_file" | awk '{print $1}');
elif command -v openssl >/dev/null 2>&1; then actual_sha=$(openssl dgst -sha256 "$product_file" | awk '{print $NF}');
else echo "pb installer: shasum, sha256sum, or openssl is required" >&2; exit 1; fi
[ "$actual_sha" = "$product_sha" ] || { echo "pb installer: product digest mismatch" >&2; exit 1; }

if [ "$os" = linux ]; then
  case "$install_dir" in /*) ;; *) echo "pb installer: --install-dir must be absolute" >&2; exit 2 ;; esac
else
  [ "$install_dir_requested" = false ] && [ -z "${PAPERBOAT_INSTALL_DIR:-}" ] || { echo "pb installer: --install-dir is not supported on macOS" >&2; exit 2; }
fi
chmod 0755 "$product_file"
installer_pb=$product_file

# Linux without systemd (a container): pb install only places the binary and pb pair prints how to run
# `pb daemon --foreground`. A reset would stop and remove a service this machine cannot have.
no_systemd=false
if [ "$os" = linux ] && [ ! -d /run/systemd/system ]; then no_systemd=true; fi

if [ "$pair" = true ] && [ "$no_systemd" = false ]; then
  host=$(hostname)
  [ -n "$host" ] || { echo "pb installer: could not resolve this machine hostname" >&2; exit 1; }
  "$installer_pb" reset --confirmation 'RESET PAPERBOAT' --hostname "$host" --enrollment-token-file "$token_file" --json > "$temporary/reset.json" || {
    status=$?
    # pb's user-facing error message (never a secret) names why reset failed; the token stays in its file.
    reason=
    if [ "$os" = darwin ] && [ -x /usr/bin/plutil ]; then
      reason=$(/usr/bin/plutil -extract error.message raw -o - "$temporary/reset.json" 2>/dev/null || true)
    elif command -v python3 >/dev/null 2>&1; then
      reason=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["error"]["message"])' "$temporary/reset.json" 2>/dev/null || true)
    fi
    echo "pb installer: pb reset failed${reason:+: $reason}; rerun this installer to retry machine enrollment" >&2
    exit "$status"
  }
  if [ "$os" = darwin ] && [ -x /usr/bin/plutil ]; then
    resume=$(/usr/bin/plutil -extract data.resume raw -o - "$temporary/reset.json")
    resume_executable=$(/usr/bin/plutil -extract data.executable raw -o - "$temporary/reset.json")
  else
    command -v python3 >/dev/null 2>&1 || { echo "pb installer: python3 is required to read the reset result" >&2; exit 1; }
    resume=$(python3 - "$temporary/reset.json" <<'PY'
import json, sys
try:
    value = json.load(open(sys.argv[1], encoding="utf-8"))
    resume = value["data"]["resume"]
    if value.get("ok") is not True or not isinstance(resume, bool): raise ValueError
    print("true" if resume else "false")
except (OSError, KeyError, TypeError, ValueError, json.JSONDecodeError):
    raise SystemExit("pb installer: reset returned invalid JSON")
PY
    )
    resume_executable=$(python3 - "$temporary/reset.json" <<'PY'
import json, sys
try:
    value = json.load(open(sys.argv[1], encoding="utf-8"))
    executable = value["data"]["executable"]
    if value.get("ok") is not True or not isinstance(executable, str): raise ValueError
    print(executable)
except (OSError, KeyError, TypeError, ValueError, json.JSONDecodeError):
    raise SystemExit("pb installer: reset returned invalid executable JSON")
PY
    )
  fi
else
  resume=false
  resume_executable=
fi

if [ "$resume" = true ]; then
  if [ -n "$resume_executable" ]; then
    case "$resume_executable" in /*) ;; *) echo "pb installer: reset returned a non-absolute resume executable" >&2; exit 1 ;; esac
    [ -x "$resume_executable" ] || { echo "pb installer: reset returned an unavailable resume executable" >&2; exit 1; }
    target=$resume_executable
  else
    target=$installer_pb
  fi
else
  install_result="$temporary/install.json"
  if [ "$os" = linux ]; then
    "$installer_pb" install --install-dir "$install_dir" --json > "$install_result"
  else
    "$installer_pb" install --json > "$install_result"
  fi || {
    status=$?
    echo "pb installer: pb install failed; rerun this installer and approve administrator access when prompted" >&2
    exit "$status"
  }
  if [ "$os" = darwin ] && [ -x /usr/bin/plutil ]; then
    target=$(/usr/bin/plutil -extract data.executable raw -o - "$install_result")
  else
    command -v python3 >/dev/null 2>&1 || { echo "pb installer: python3 is required to read the installed executable path" >&2; exit 1; }
    target=$(python3 - "$install_result" <<'PY'
import json, sys
try:
    value = json.load(open(sys.argv[1], encoding="utf-8"))
    executable = value["data"]["executable"]
    if value.get("ok") is not True or not isinstance(executable, str):
        raise ValueError
    print(executable)
except (OSError, KeyError, TypeError, ValueError, json.JSONDecodeError):
    raise SystemExit("pb installer: install returned invalid JSON")
PY
    )
  fi
fi
case "$target" in /*) ;; *) echo "pb installer: install returned a non-absolute executable path" >&2; exit 1 ;; esac
[ -x "$target" ] || { echo "pb installer: installed executable is unavailable at $target" >&2; exit 1; }
[ "$resume" = true ] || echo "Installed pb to $target" >&2

set --
[ -z "$machine_name" ] || set -- "$@" --name "$machine_name"
if [ "$recipe" = true ]; then
  # pb pair reads PB_RECIPE; an already paired machine is left as it is.
  "$target" pair "$@"
elif [ "$pair" = true ]; then
  [ -z "$token_file" ] || set -- "$@" --enrollment-token-file "$token_file"
  [ -z "$ssh_port" ] || set -- "$@" --ssh-port "$ssh_port"
  "$target" pair "$@"
elif [ "$setup" = true ]; then
  [ -z "$ssh_port" ] || set -- "$@" --ssh-port "$ssh_port"
  [ -z "$recovery_output" ] || set -- "$@" --recovery-output "$recovery_output"
  "$target" setup "$@"
elif [ "$join" = true ]; then
  # No token: continue into the browser approval, which adds this computer to the account.
  "$target" login
else
  "$target" --version
fi
}

main "$@"
