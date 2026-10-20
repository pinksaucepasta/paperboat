#!/bin/sh
# F1/F3: the Paperboat daemon survives logout on a fresh systemd host, and uninstall restores the
# lingering setting. Uses a build of the CLI for the container's architecture:
#   DOCKER="sudo -n docker" tools/qualify-logout-survival.sh --pb /path/to/pb [--base ubuntu:24.04]
set -eu
base=ubuntu:24.04 platform='' pb=''
while [ "$#" -gt 0 ]; do
 case "$1" in
  --base) base=$2; shift 2;; --platform) platform=$2; shift 2;; --pb) pb=$2; shift 2;;
  *) echo "unknown argument: $1" >&2; exit 2;;
 esac
done
[ -x "$pb" ] || { echo '--pb must name an executable pb build' >&2; exit 2; }
docker=${DOCKER:-docker}
repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
mkdir -p "$repo/.build"
work=$(mktemp -d "$repo/.build/qualify-logout.XXXXXX")
name=pb-logout-$(basename "$work" | tr 'A-Z.' 'a-z-')
step() { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
. "$repo/tools/fresh-host.sh"
trap 'fresh_host_down; rm -rf "$work"' EXIT HUP INT TERM

step "fresh $base host, user without lingering"
fresh_host_up
$docker cp "$pb" "$name:/home/tester/pb"
inside 'chown tester /home/tester/pb; chmod 755 /home/tester/pb; loginctl disable-linger tester; rm -f /var/lib/systemd/linger/tester'

step "pb install enables lingering and doctor says so"
inside_user './pb install' > "$work/install" 2>&1 || { tail -20 "$work/install"; fail "install failed"; }
inside 'test -e /var/lib/systemd/linger/tester' || fail "install did not enable lingering"
inside_user '~/.local/bin/pb doctor 2>&1 | grep -q "logout_survival: pass"' || fail "doctor does not report logout survival"

step "the daemon keeps running after every login session ends"
inside 'for s in $(loginctl list-sessions --no-legend | awk "\$3==\"tester\" {print \$1}"); do loginctl terminate-session "$s"; done; sleep 6'
inside 'systemctl is-active --quiet user@1001.service' || fail "the user manager stopped at logout"
inside 'su tester -s /bin/sh -c "XDG_RUNTIME_DIR=/run/user/1001 systemctl --user is-active --quiet paperboatd.service"' || fail "paperboatd stopped at logout"

step "uninstall restores the lingering setting"
inside_user '~/.local/bin/pb uninstall --confirmation "UNINSTALL PAPERBOAT" --hostname "$(hostname)"' > "$work/uninstall" 2>&1 || { tail -20 "$work/uninstall"; fail "uninstall failed"; }
inside '! test -e /var/lib/systemd/linger/tester' || fail "uninstall left lingering enabled"
echo "PASS logout survival on $base ($(inside 'uname -m'))"
