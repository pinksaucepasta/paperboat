#!/bin/sh
# F2-05: a machine removed from the account loses access at once and says so; its credential
# refresh does not spin. A throwaway computer (fresh systemd container) is paired with a build of
# the CLI, revoked from an owner computer by its exact name, and then must report it clearly.
#   DOCKER="sudo -n docker" tools/qualify-revoked-machine.sh --pb /path/to/pb --owner anvit@hp
# The owner computer needs a signed-in pb (key ssh); nothing but the throwaway's own record is touched.
set -eu
base=ubuntu:24.04 platform='' pb='' owner=''
while [ "$#" -gt 0 ]; do
 case "$1" in
  --base) base=$2; shift 2;; --platform) platform=$2; shift 2;; --pb) pb=$2; shift 2;; --owner) owner=$2; shift 2;;
  *) echo "unknown argument: $1" >&2; exit 2;;
 esac
done
[ -x "$pb" ] || { echo '--pb must name an executable pb build for the container architecture' >&2; exit 2; }
[ -n "$owner" ] || { echo '--owner user@host is required' >&2; exit 2; }
docker=${DOCKER:-docker}
repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
mkdir -p "$repo/.build"
work=$(mktemp -d "$repo/.build/qualify-revoked.XXXXXX")
name=pb-revoked-$(basename "$work" | tr 'A-Z.' 'a-z-')
machine=lanea-revoked-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
step() { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
. "$repo/tools/fresh-host.sh"
owner_pb() { ssh -o BatchMode=yes "$owner" "~/.local/bin/pb $*"; }
cleanup() {
 # Remove the throwaway's record by its exact name if the run stopped before revoking it.
 if [ -n "${machine_created:-}" ] && ! [ -n "${revoked:-}" ]; then
  code=$(owner_pb machine revoke "$machine" --json 2>/dev/null | python3 -c 'import sys,json;d=json.load(sys.stdin);d=d.get("data",d);print(d.get("confirmation_token",""))' 2>/dev/null || true)
  [ -z "$code" ] || owner_pb machine revoke "$machine" --confirm "$code" --json >/dev/null 2>&1 || true
 fi
 fresh_host_down; rm -rf "$work"
}
trap cleanup EXIT HUP INT TERM

step "fresh $base host"
fresh_host_up
$docker cp "$pb" "$name:/home/tester/pb"
inside 'chown tester /home/tester/pb; chmod 755 /home/tester/pb'

step "enrollment token for $machine (never printed)"
owner_pb machine add --name "$machine" --json | python3 -c '
import sys,json,re
d=json.load(sys.stdin); d=d.get("data",d)
m=re.search(r"[?&]p=([^\x27\"& ]+)",d["linux_macos_command"])
sys.stdout.write(m.group(1)[-26:])' > "$work/token"
[ -s "$work/token" ] || fail "no enrollment token"
$docker cp "$work/token" "$name:/home/tester/token"
inside 'chown tester /home/tester/token; chmod 600 /home/tester/token'
machine_created=1

step "install and pair the throwaway"
inside_user './pb install' > "$work/install" 2>&1 || { tail -15 "$work/install"; fail "install failed"; }
inside_user "~/.local/bin/pb pair --enrollment-token-file /home/tester/token --name $machine" > "$work/pair" 2>&1 || { tail -15 "$work/pair"; fail "pair failed"; }
i=0
until owner_pb machine list --json | python3 -c '
import sys,json
want=sys.argv[1]
sys.exit(0 if any(m["alias"]==want and m["online"] for m in json.load(sys.stdin)["machines"]) else 1)' "$machine"; do
 i=$((i+1)); [ "$i" -le 30 ] || fail "$machine did not come online"; sleep 4
done
inside_user '~/.local/bin/pb status' | grep -q 'Daemon: ready' || fail "throwaway is not ready before revoke"

step "owner revokes $machine by exact name"
code=$(owner_pb machine revoke "$machine" --json | python3 -c 'import sys,json;d=json.load(sys.stdin);d=d.get("data",d);print(d["confirmation_token"])')
owner_pb machine revoke "$machine" --confirm "$code" --json > "$work/revoke" || fail "revoke failed"
revoked=1
start=$(date +%s)

step "pb status says so within 6 minutes"
ok=''
for _ in $(seq 1 120); do
 inside_user '~/.local/bin/pb status 2>&1' > "$work/status" || true
 if grep -qiE 'removed|signed out|no longer valid' "$work/status" && grep -q 'pb login' "$work/status"; then ok=1; break; fi
 sleep 3
done
[ -n "$ok" ] || { head -6 "$work/status"; inside_user '~/.local/bin/pb doctor 2>&1 | head -8'; fail "pb status does not say the computer was removed and point to pb login"; }
echo "status explained after $(( $(date +%s) - start )) s"

step "pb doctor says the computer was removed from the account"
inside_user '~/.local/bin/pb doctor 2>&1' > "$work/doctor" || true
grep -q 'removed from the account' "$work/doctor" || { cat "$work/doctor"; fail "pb doctor does not say the computer was removed from the account"; }
grep -q 'pb login' "$work/doctor" || fail "pb doctor does not point to pb login"

step "no tight retry loop: bounded failed requests over 60 s"
inside_user 'ls ~/.local/state/paperboat/diagnostics >/dev/null 2>&1; cat ~/.local/state/paperboat/diagnostics/events-*.ndjson 2>/dev/null | grep -c control_request_failed' > "$work/before" || true
sleep 60
inside_user 'cat ~/.local/state/paperboat/diagnostics/events-*.ndjson 2>/dev/null | grep -c control_request_failed' > "$work/after" || true
growth=$(( $(tr -d '\n' < "$work/after" | sed 's/^$/0/') - $(tr -d '\n' < "$work/before" | sed 's/^$/0/') ))
echo "failed control requests in 60 s: $growth"
[ "$growth" -le 12 ] || fail "the revoked computer retries too often: $growth failed requests in 60 s"
inside_user 'systemctl --user is-active --quiet paperboatd.service' || fail "daemon stopped instead of waiting signed out"
echo "PASS revoked machine says so and does not spin ($machine, statement after $(( $(date +%s) - start )) s)"
