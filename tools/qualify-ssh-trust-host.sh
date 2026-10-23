#!/bin/sh
# C066: a changed SSH host key is refused, `pb ssh trust-host --fingerprint` accepts exactly the pending one,
# and managed SSH works again. A throwaway container machine (with sshd) is the target; its host keys are
# regenerated inside the container, so no real machine's SSH identity is touched.
#   DOCKER="sudo -n docker" tools/qualify-ssh-trust-host.sh --pb /path/to/arm64/pb --owner-command "ssh -o BatchMode=yes anvit@hp"
# The owner computer runs pb ssh; it needs a signed-in pb. --owner-command is how a shell on it is reached
# (its arguments are one command string); the throwaway's record is revoked at the end by its exact name.
set -eu
base=ubuntu:24.04 platform='' pb='' owner_command=''
while [ "$#" -gt 0 ]; do
 case "$1" in
  --base) base=$2; shift 2;; --platform) platform=$2; shift 2;; --pb) pb=$2; shift 2;;
  --owner-command) owner_command=$2; shift 2;;
  *) echo "unknown argument: $1" >&2; exit 2;;
 esac
done
[ -x "$pb" ] || { echo '--pb must name an executable pb build for the container architecture' >&2; exit 2; }
[ -n "$owner_command" ] || { echo '--owner-command is required' >&2; exit 2; }
docker=${DOCKER:-docker}
repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
mkdir -p "$repo/.build"
work=$(mktemp -d "$repo/.build/qualify-trust.XXXXXX")
name=pb-trust-$(basename "$work" | tr 'A-Z.' 'a-z-')
machine=lanea-trust-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
step() { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
. "$repo/tools/fresh-host.sh"
owner_pb() { $owner_command "~/.local/bin/pb $*"; }
cleanup() {
 if [ -n "${machine_created:-}" ]; then
  code=$(owner_pb machine revoke "$machine" --json 2>/dev/null | python3 -c 'import sys,json;d=json.load(sys.stdin);d=d.get("data",d);print(d.get("confirmation_token",""))' 2>/dev/null || true)
  [ -z "$code" ] || owner_pb machine revoke "$machine" --confirm "$code" --json >/dev/null 2>&1 || true
 fi
 fresh_host_down; rm -rf "$work"
}
trap cleanup EXIT HUP INT TERM

step "fresh $base host with sshd"
fresh_host_up
inside 'DEBIAN_FRONTEND=noninteractive apt-get update -qq >/dev/null 2>&1 && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq openssh-server >/dev/null 2>&1 && systemctl enable --now ssh >/dev/null 2>&1; ss -ltn | grep -q ":22 "' || fail "sshd is not listening in the throwaway"
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
# A pairing that stops after enrollment is retried with the same token file, as its own message says.
attempt=0
until inside_user "~/.local/bin/pb pair --enrollment-token-file /home/tester/token --name $machine" > "$work/pair" 2>&1; do
 attempt=$((attempt+1)); [ "$attempt" -lt 3 ] || { tail -8 "$work/pair"; inside 'journalctl -u paperboat-carrier-u1001.service --no-pager -n 14 2>/dev/null | cut -c1-260' || true; inside_user '~/.local/bin/pb doctor 2>&1 | head -8' || true; fail "pair failed after $attempt attempts"; }
 sleep 5
done
i=0
until owner_pb ssh "$machine" -- id -un > "$work/first" 2>&1; do
 i=$((i+1)); [ "$i" -le 30 ] || { tail -5 "$work/first"; fail "managed SSH to $machine never worked"; }; sleep 4
done
grep -q tester "$work/first" || fail "the first managed SSH session did not run as the target user"

step "the target's host keys change"
inside 'rm -f /etc/ssh/ssh_host_* && ssh-keygen -A >/dev/null && systemctl restart ssh'

step "pb ssh refuses the changed identity and names the pending one"
i=0
until ! owner_pb ssh "$machine" -- id -un > "$work/refused" 2>&1 && grep -q 'pb ssh trust-host' "$work/refused"; do
 i=$((i+1)); [ "$i" -le 20 ] || { tail -c 400 "$work/refused"; fail "pb ssh did not refuse the changed identity with the trust-host command"; }; sleep 6
done
# The trusted value is the server's fingerprint of the pending host-key set, as pb ssh printed it.
new=$(sed -n 's/.*pb ssh trust-host [^ ]* --fingerprint \(SHA256:[A-Za-z0-9+\/=_-]*\).*/\1/p' "$work/refused" | head -1)
case "$new" in SHA256:*) ;; *) fail "the refusal did not name the pending fingerprint";; esac

step "a wrong fingerprint is rejected"
if owner_pb ssh trust-host "$machine" --fingerprint SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA > "$work/wrong" 2>&1; then fail "trust-host accepted a fingerprint that is not pending"; fi

step "the exact pending fingerprint is accepted"
owner_pb ssh trust-host "$machine" --fingerprint "$new" > "$work/trust" 2>&1 || { head -c 400 "$work/trust"; fail "trust-host rejected the exact pending fingerprint"; }
owner_pb ssh "$machine" -- id -un > "$work/after" 2>&1 || { head -c 400 "$work/after"; fail "managed SSH does not work after trust-host"; }
grep -q tester "$work/after" || fail "SSH after trust-host did not run as the target user"
echo "PASS changed host key refused, wrong fingerprint rejected, exact fingerprint trusted ($machine)"
