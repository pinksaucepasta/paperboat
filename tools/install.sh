#!/bin/sh
set -eu

# The release-published copy pins a small verifier by immutable GitHub URL,
# length, and SHA-256. That verifier checks TUF before downloading pb once.
repository=${PAPERBOAT_GITHUB_REPOSITORY:-@PAPERBOAT_BOOTSTRAP_REPOSITORY@}
requested_version=${PAPERBOAT_VERSION:-latest}
tuf_url=${PAPERBOAT_TUF_URL:-https://get.pprbt.dev/tuf}
bootstrap_version='@PAPERBOAT_BOOTSTRAP_VERSION@'
install_dir=${PAPERBOAT_INSTALL_DIR:-"${HOME}/.local/bin"}
install_dir_requested=false
setup=false
pair=false
token=${PAPERBOAT_ENROLLMENT_TOKEN:-}
token_file=
machine_name=${PAPERBOAT_MACHINE_ALIAS:-${PAPERBOAT_MACHINE_NAME:-}}
ssh_port=
recovery_output=
[ -z "$token" ] || pair=true

usage() { cat <<'EOF'
Install the current Paperboat release.

Usage: install.sh [options]
  --version VERSION             Require this signed release version
  --install-dir DIRECTORY       Install Linux pb here (default: ~/.local/bin)
  --setup                       Run pb setup after installation
  --pair                        Run pb pair after installation
  --enrollment-token TOKEN      Use a dashboard-issued pairing token
  --enrollment-token-file FILE  Read a pairing token from an absolute file
  --name NAME                   Set the machine name during setup or pairing
  --ssh-port PORT               Existing SSH port used by managed SSH
  --recovery-output FILE        Save the recovery key during setup
  --no-setup                    Install only (the default)
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
    --no-setup) setup=false; pair=false; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "pb installer: unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[ "$setup" != true ] || [ "$pair" != true ] || { echo "pb installer: use either --setup or --pair" >&2; exit 2; }
[ -z "$token" ] || [ -z "$token_file" ] || { echo "pb installer: use only one enrollment token source" >&2; exit 2; }
[ "$pair" != true ] || [ -n "$token" ] || [ -n "$token_file" ] || { echo "pb installer: --pair requires --enrollment-token or --enrollment-token-file" >&2; exit 2; }
[ -z "$ssh_port" ] || [ "$setup" = true ] || [ "$pair" = true ] || { echo "pb installer: --ssh-port requires --setup or --pair" >&2; exit 2; }
[ -z "$recovery_output" ] || [ "$setup" = true ] || { echo "pb installer: --recovery-output requires --setup" >&2; exit 2; }

case $(uname -s) in Darwin) os=darwin ;; Linux) os=linux ;; *) echo "pb installer: only macOS and Linux are supported" >&2; exit 1 ;; esac
case $(uname -m) in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) echo "pb installer: unsupported architecture: $(uname -m)" >&2; exit 1 ;; esac
[ "$os" != darwin ] || [ "$arch" = arm64 ] || { echo "pb installer: macOS releases support arm64" >&2; exit 1; }

case "$repository" in */*) owner=${repository%%/*}; repo_name=${repository#*/} ;; *) owner=; repo_name= ;; esac
case "$owner$repo_name" in ""|*[!A-Za-z0-9_.-]*) echo "pb installer: invalid GitHub repository" >&2; exit 1 ;; esac
[ "$repository" = "$owner/$repo_name" ] || { echo "pb installer: invalid GitHub repository" >&2; exit 1; }
case "$tuf_url" in https://*) ;; *) echo "pb installer: TUF URL must use HTTPS" >&2; exit 1 ;; esac
command -v curl >/dev/null 2>&1 || { echo "pb installer: curl is required" >&2; exit 1; }

temporary=$(mktemp -d "${TMPDIR:-/tmp}/paperboat-install.XXXXXX")
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
if [ "$os" = darwin ]; then asset="$asset.pkg"; fi
bootstrap_asset="pb-bootstrap-${os}-${arch}"
case "$os-$arch" in
  linux-amd64) bootstrap_sha='@PAPERBOAT_BOOTSTRAP_LINUX_AMD64_SHA256@'; bootstrap_length='@PAPERBOAT_BOOTSTRAP_LINUX_AMD64_LENGTH@' ;;
  linux-arm64) bootstrap_sha='@PAPERBOAT_BOOTSTRAP_LINUX_ARM64_SHA256@'; bootstrap_length='@PAPERBOAT_BOOTSTRAP_LINUX_ARM64_LENGTH@' ;;
  darwin-arm64) bootstrap_sha='@PAPERBOAT_BOOTSTRAP_DARWIN_ARM64_SHA256@'; bootstrap_length='@PAPERBOAT_BOOTSTRAP_DARWIN_ARM64_LENGTH@' ;;
esac
case "$bootstrap_version:$bootstrap_sha:$bootstrap_length" in *'@PAPERBOAT_'*) echo 'pb installer: unpublished bootstrap; use the release installer' >&2; exit 1 ;; esac
bootstrap_url="https://github.com/${repository}/releases/download/${bootstrap_version}/${bootstrap_asset}"
verifier="$temporary/$bootstrap_asset"
curl --fail --location --show-error --silent --connect-timeout 15 --max-time 300 --proto '=https' --proto-redir '=https' "$bootstrap_url" -o "$verifier"
[ "$(wc -c < "$verifier" | tr -d ' ')" = "$bootstrap_length" ] || { echo "pb installer: bootstrap verifier length mismatch" >&2; exit 1; }
if command -v shasum >/dev/null 2>&1; then actual_sha=$(shasum -a 256 "$verifier" | awk '{print $1}');
elif command -v sha256sum >/dev/null 2>&1; then actual_sha=$(sha256sum "$verifier" | awk '{print $1}');
elif command -v openssl >/dev/null 2>&1; then actual_sha=$(openssl dgst -sha256 "$verifier" | awk '{print $NF}');
else echo "pb installer: shasum, sha256sum, or openssl is required" >&2; exit 1; fi
[ "$actual_sha" = "$bootstrap_sha" ] || { echo "pb installer: bootstrap verifier digest mismatch" >&2; exit 1; }
chmod 0700 "$verifier"
"$verifier" --tuf-url "$tuf_url" --state-dir "$temporary/tuf" --github-repository "$repository" --version "$requested_version" > "$temporary/verified.json"
if [ "$os" = darwin ] && [ -x /usr/bin/plutil ]; then
  download=$(/usr/bin/plutil -extract path raw -o - "$temporary/verified.json")
else
  command -v python3 >/dev/null 2>&1 || { echo 'pb installer: python3 is required to read verifier output' >&2; exit 1; }
  download=$(python3 - "$temporary/verified.json" <<'PY'
import json, os, sys
value = json.load(open(sys.argv[1], encoding='utf-8'))
path = value.get('path')
if not isinstance(path, str) or not os.path.isabs(path): raise SystemExit('pb installer: verifier returned an invalid path')
print(path)
PY
  )
fi
[ "$download" = "$temporary/tuf/product/$asset" ] && [ -f "$download" ] && [ ! -L "$download" ] || { echo 'pb installer: verifier returned an unexpected artifact' >&2; exit 1; }

if [ "$os" = linux ]; then
  case "$install_dir" in /*) ;; *) echo "pb installer: --install-dir must be absolute" >&2; exit 2 ;; esac
  chmod 0755 "$download"
  installer_pb=$download
else
  [ "$install_dir_requested" = false ] && [ -z "${PAPERBOAT_INSTALL_DIR:-}" ] || { echo "pb installer: --install-dir is not supported on macOS" >&2; exit 2; }
  command -v pkgutil >/dev/null 2>&1 && command -v cpio >/dev/null 2>&1 || { echo "pb installer: pkgutil and cpio are required" >&2; exit 1; }
  expanded="$temporary/expanded"; payload="$temporary/payload"
  pkgutil --expand "$download" "$expanded"
  mkdir -p "$payload"
  (cd "$payload" && gzip -dc "$expanded/Payload" | cpio -idm >/dev/null 2>&1)
  payload_pb="$payload/Library/PrivilegedHelperTools/Paperboat/bin/pb"
  [ -f "$payload_pb" ] && [ ! -L "$payload_pb" ] || { echo "pb installer: macOS package is missing its canonical executable" >&2; exit 1; }
  chmod 0755 "$payload_pb"
  installer_pb=$payload_pb
fi

if [ "$pair" = true ]; then
  unset PAPERBOAT_ENROLLMENT_TOKEN || true
  host=$(hostname)
  [ -n "$host" ] || { echo "pb installer: could not resolve this machine hostname" >&2; exit 1; }
  "$installer_pb" reset --confirmation 'RESET PAPERBOAT' --hostname "$host" --enrollment-token-file "$token_file" --json > "$temporary/reset.json"
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
  if [ "$os" = linux ]; then "$installer_pb" install --install-dir "$install_dir" --json > "$install_result"; else "$installer_pb" install --json > "$install_result"; fi
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
if [ "$pair" = true ]; then
  [ -z "$token_file" ] || set -- "$@" --enrollment-token-file "$token_file"
  [ -z "$ssh_port" ] || set -- "$@" --ssh-port "$ssh_port"
  "$target" pair "$@"
elif [ "$setup" = true ]; then
  [ -z "$ssh_port" ] || set -- "$@" --ssh-port "$ssh_port"
  [ -z "$recovery_output" ] || set -- "$@" --recovery-output "$recovery_output"
  "$target" setup "$@"
else
  "$target" --version
fi
