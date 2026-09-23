#!/bin/sh
set -eu

repository_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
temporary=$(mktemp -d "${TMPDIR:-/tmp}/paperboat-macos-install.XXXXXX")
trap 'rm -rf "$temporary"' EXIT HUP INT TERM
mkdir -p "$temporary/bin" "$temporary/home" "$temporary/state"
mkdir -p "$temporary/package/Library/PrivilegedHelperTools/Paperboat/bin"
printf '#!/bin/sh\nprintf "%%s\\n" "$*" >> "$PAPERBOAT_TEST_PB_LOG"\nexit 97\n' > "$temporary/package/Library/PrivilegedHelperTools/Paperboat/bin/pb"
chmod 0755 "$temporary/package/Library/PrivilegedHelperTools/Paperboat/bin/pb"
(cd "$temporary/package" && find . -type f | cpio -o -H odc 2>/dev/null | gzip -c) > "$temporary/Payload"
cat > "$temporary/verifier" <<'EOF'
#!/bin/sh
set -eu
while [ "$#" -gt 0 ]; do
  case "$1" in --state-dir) state=$2; shift 2 ;; *) shift ;; esac
done
mkdir -p "$state/product"
printf 'pkg\n' > "$state/product/pb-darwin-arm64.pkg"
printf '{"path":"%s","version":"2026.09.02.0"}\n' "$state/product/pb-darwin-arm64.pkg"
EOF
chmod 0700 "$temporary/verifier"
verifier_sha=$(sha256sum "$temporary/verifier" | awk '{print $1}')
verifier_length=$(wc -c < "$temporary/verifier" | tr -d ' ')
installer="$temporary/install"
sed -e 's/@PAPERBOAT_BOOTSTRAP_VERSION@/2026.09.02.0/g' \
  -e 's|@PAPERBOAT_BOOTSTRAP_REPOSITORY@|example/paperboat-cli|g' \
  -e "s/@PAPERBOAT_BOOTSTRAP_DARWIN_ARM64_SHA256@/$verifier_sha/g" \
  -e "s/@PAPERBOAT_BOOTSTRAP_DARWIN_ARM64_LENGTH@/$verifier_length/g" \
  "$repository_root/tools/install.sh" > "$installer"
chmod 0700 "$installer"

cat > "$temporary/bin/uname" <<'EOF'
#!/bin/sh
case "${1:-}" in
  -s) echo Darwin ;;
  -m) echo arm64 ;;
  *) exit 2 ;;
esac
EOF
cat > "$temporary/bin/curl" <<'EOF'
#!/bin/sh
set -eu
output=
url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o|--output) output=$2; shift 2 ;;
    --proto|--retry|--retry-delay|--connect-timeout|--max-time) shift 2 ;;
    --*) shift ;;
    *) url=$1; shift ;;
  esac
done
printf '%s\n' "$url" >> "$PAPERBOAT_TEST_CURL_LOG"
case "$url" in
  https://github.com/example/paperboat-cli/releases/download/2026.09.02.0/pb-bootstrap-darwin-arm64)
    [ -n "$output" ] || exit 1
    cp "$PAPERBOAT_TEST_VERIFIER" "$output"
    exit 0
    ;;
  *) echo "unexpected curl URL: $url" >&2; exit 1 ;;
esac
exit 1
EOF
cat > "$temporary/bin/pkgutil" <<'EOF'
#!/bin/sh
set -eu
test "$1" = --expand
mkdir -p "$3"
cp "$PAPERBOAT_TEST_PAYLOAD" "$3/Payload"
EOF
cat > "$temporary/bin/id" <<'EOF'
#!/bin/sh
case "${1:-}" in
  -u) echo 1000 ;;
  *) exec /usr/bin/id "$@" ;;
esac
EOF
cat > "$temporary/bin/launchctl" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$PAPERBOAT_TEST_LAUNCHCTL_LOG"
for argument do
  case "$argument" in
    system/com.pinksaucepasta.paperboat.hostd) /bin/rm -f "$PAPERBOAT_TEST_STATE/hostd.plist" ;;
    system/com.pinksaucepasta.paperboat.updated) /bin/rm -f "$PAPERBOAT_TEST_STATE/updated.plist" ;;
  esac
done
exit 0
EOF
cat > "$temporary/bin/rm" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$PAPERBOAT_TEST_RM_LOG"
protected=false
for argument do
  case "$argument" in
    /Library/LaunchDaemons/com.pinksaucepasta.paperboat.hostd.plist|/Library/LaunchDaemons/com.pinksaucepasta.paperboat.updated.plist|/Library/PrivilegedHelperTools/Paperboat|/Library/Application\ Support/Paperboat|/usr/local/bin/pb|/usr/local/libexec/paperboat/pb|/var/run/paperboat-hostd/hostd.sock|/var/run/paperboat-updated/control.sock)
      protected=true
      ;;
  esac
done
if [ "$protected" = true ]; then
  for argument do
    case "$argument" in
      /Library/LaunchDaemons/com.pinksaucepasta.paperboat.hostd.plist) /bin/rm -f "$PAPERBOAT_TEST_STATE/hostd.plist" ;;
      /Library/LaunchDaemons/com.pinksaucepasta.paperboat.updated.plist) /bin/rm -f "$PAPERBOAT_TEST_STATE/updated.plist" ;;
      /Library/PrivilegedHelperTools/Paperboat) /bin/rm -rf "$PAPERBOAT_TEST_STATE/helper" ;;
      /Library/Application\ Support/Paperboat) /bin/rm -rf "$PAPERBOAT_TEST_STATE/application-support" ;;
      /usr/local/bin/pb) /bin/rm -f "$PAPERBOAT_TEST_STATE/cli" ;;
      /usr/local/libexec/paperboat/pb) /bin/rm -f "$PAPERBOAT_TEST_STATE/legacy-helper" ;;
      /var/run/paperboat-hostd/hostd.sock) /bin/rm -f "$PAPERBOAT_TEST_STATE/hostd.sock" ;;
      /var/run/paperboat-updated/control.sock) /bin/rm -f "$PAPERBOAT_TEST_STATE/updated.sock" ;;
    esac
  done
  exit 0
fi
exec /bin/rm "$@"
EOF
cat > "$temporary/bin/sudo" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$PAPERBOAT_TEST_SUDO_LOG"
if [ "${1:-}" = -n ]; then
  shift
fi
case "${1:-}" in
  true) exit 0 ;;
  launchctl) exec "$PAPERBOAT_TEST_FAKE_BIN/launchctl" "$@" ;;
  rm) exec "$PAPERBOAT_TEST_FAKE_BIN/rm" "$@" ;;
  installer) exit 97 ;;
  *) echo "unexpected sudo command: $*" >&2; exit 1 ;;
esac
EOF
chmod 0700 "$temporary/bin/uname" "$temporary/bin/curl" "$temporary/bin/pkgutil" "$temporary/bin/id" "$temporary/bin/launchctl" "$temporary/bin/rm" "$temporary/bin/sudo"

export PAPERBOAT_TEST_STATE="$temporary/state"
export PAPERBOAT_TEST_FAKE_BIN="$temporary/bin"
export PAPERBOAT_TEST_VERIFIER="$temporary/verifier"
export PAPERBOAT_TEST_PAYLOAD="$temporary/Payload"
export PAPERBOAT_TEST_PB_LOG="$temporary/pb.log"

seed_managed_state() {
  /bin/rm -rf "$PAPERBOAT_TEST_STATE"
  mkdir -p "$PAPERBOAT_TEST_STATE/helper/bin" "$PAPERBOAT_TEST_STATE/application-support"
  touch "$PAPERBOAT_TEST_STATE/hostd.plist" "$PAPERBOAT_TEST_STATE/updated.plist" \
    "$PAPERBOAT_TEST_STATE/helper/bin/pb" "$PAPERBOAT_TEST_STATE/application-support/config" \
    "$PAPERBOAT_TEST_STATE/cli" "$PAPERBOAT_TEST_STATE/legacy-helper" \
    "$PAPERBOAT_TEST_STATE/hostd.sock" "$PAPERBOAT_TEST_STATE/updated.sock"
}

run_installer() {
  scenario=$1
  shift
  PAPERBOAT_TEST_CURL_LOG="$temporary/$scenario-curl.log" \
  PAPERBOAT_TEST_LAUNCHCTL_LOG="$temporary/$scenario-launchctl.log" \
  PAPERBOAT_TEST_RM_LOG="$temporary/$scenario-rm.log" \
  PAPERBOAT_TEST_SUDO_LOG="$temporary/$scenario-sudo.log" \
  PAPERBOAT_GITHUB_REPOSITORY=example/paperboat-cli \
  HOME="$temporary/home" \
  PATH="$temporary/bin:/usr/bin:/bin" \
  "$installer" "$@" >"$temporary/$scenario-output" 2>"$temporary/$scenario-error"
}

seed_managed_state
if run_installer install-only; then
  echo 'install-only test expected the fake package installer to fail' >&2
  exit 1
fi
for marker in hostd.plist updated.plist cli legacy-helper hostd.sock updated.sock; do
  test -e "$PAPERBOAT_TEST_STATE/$marker" || { echo "install-only removed $marker" >&2; exit 1; }
done
test -d "$PAPERBOAT_TEST_STATE/helper"
test -f "$PAPERBOAT_TEST_STATE/helper/bin/pb"
test -d "$PAPERBOAT_TEST_STATE/application-support"
test -f "$PAPERBOAT_TEST_STATE/application-support/config"
if grep -Eq 'launchctl bootout|com\.pinksaucepasta\.paperboat\.(hostd|updated)|/Library/PrivilegedHelperTools/Paperboat|/Library/Application Support/Paperboat|/var/run/paperboat-(hostd|updated)|/usr/local/bin/pb' "$temporary/install-only-sudo.log" "$temporary/install-only-rm.log" "$temporary/install-only-launchctl.log" 2>/dev/null; then
  echo 'install-only called macOS service cleanup' >&2
  exit 1
fi

seed_managed_state
if run_installer explicit-setup --setup; then
  echo 'explicit setup test expected the fake package installer to fail' >&2
  exit 1
fi
grep -q '^install --json$' "$PAPERBOAT_TEST_PB_LOG"
for marker in hostd.plist updated.plist cli legacy-helper hostd.sock updated.sock; do
  test -e "$PAPERBOAT_TEST_STATE/$marker" || { echo "failed install removed $marker" >&2; exit 1; }
done

echo 'macOS install preservation: ok'
