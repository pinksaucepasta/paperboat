#!/bin/sh
# S3-02 on Linux: the published `pb` one-liner installs the official release on a fresh host.
#   PAPERBOAT_INSTALLER_URL=https://<get host>/install tools/qualify-pb-installer.sh \
#     --base ubuntu:24.04 --expect-version 2026.10.19.7 [--platform linux/amd64]
# Fresh host = throwaway systemd container (tools/fresh-host.sh). Everything is removed on exit.
set -eu
base=ubuntu:24.04 platform='' expect=''
while [ "$#" -gt 0 ]; do
 case "$1" in
  --base) base=$2; shift 2;; --platform) platform=$2; shift 2;; --expect-version) expect=$2; shift 2;;
  *) echo "unknown argument: $1" >&2; exit 2;;
 esac
done
: "${PAPERBOAT_INSTALLER_URL:?set the published installer URL}"
[ -n "$expect" ] || { echo '--expect-version is required' >&2; exit 2; }
docker=${DOCKER:-docker}
repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
mkdir -p "$repo/.build"
work=$(mktemp -d "$repo/.build/qualify-pb.XXXXXX")
name=pb-qualify-$(basename "$work" | tr 'A-Z.' 'a-z-')
step() { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
. "$repo/tools/fresh-host.sh"
trap 'fresh_host_down; rm -rf "$work"' EXIT HUP INT TERM

step "fresh $base host"
fresh_host_up
one_liner="curl -fsSL '$PAPERBOAT_INSTALLER_URL' | sh -s -- --no-setup"

step "published one-liner installs the release"
inside_user "$one_liner" > "$work/out" 2>&1 || { tail -30 "$work/out"; fail "one-liner failed"; }
inside_user '~/.local/bin/pb --version' | grep -q "Version $expect" || fail "installed version is not $expect"
inside_user 'systemctl --user is-active --quiet paperboatd.service' || fail "daemon is not running"
inside 'systemctl is-active --quiet paperboat-machineguard.service' || fail "machine guard is not running"
inside_user '~/.local/bin/pb doctor --json' > "$work/doctor" 2>&1 || true
grep -q '"checks"\|"name"' "$work/doctor" || { head -c 400 "$work/doctor"; fail "pb doctor did not report"; }

step "the default ending of the one-liner reaches the browser approval link"
# The bare one-liner continues into `pb login`; on a fresh host it must print an approval link.
inside_user '~/.local/bin/pb login --no-browser' > "$work/login" 2>&1 &
login_pid=$!
for _ in $(seq 1 20); do grep -q 'https://' "$work/login" 2>/dev/null && break; sleep 1; done
grep -q 'https://' "$work/login" || { head -c 400 "$work/login"; kill "$login_pid" 2>/dev/null || true; fail "pb login printed no approval link"; }
kill "$login_pid" 2>/dev/null || true
wait "$login_pid" 2>/dev/null || true

step "re-running the one-liner keeps a working install"
inside_user "$one_liner" > "$work/out2" 2>&1 || { tail -30 "$work/out2"; fail "re-run failed"; }
inside_user '~/.local/bin/pb --version' | grep -q "Version $expect" || fail "version changed on re-run"
inside_user 'systemctl --user is-active --quiet paperboatd.service' || fail "daemon stopped after re-run"

step "pb uninstall removes it"
inside_user '~/.local/bin/pb uninstall --confirmation "UNINSTALL PAPERBOAT" --hostname "$(hostname)"' > "$work/out3" 2>&1 || { tail -20 "$work/out3"; fail "uninstall failed"; }
inside_user '! test -e ~/.local/bin/pb' || fail "uninstall left the executable"
inside '! systemctl is-active --quiet paperboat-machineguard.service' || fail "uninstall left the machine guard"
echo "PASS pb one-liner on $base ($(inside 'uname -m'))"
