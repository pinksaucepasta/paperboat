#!/bin/sh
# F6-05 (full disk): a receiver whose Inbox cannot hold a send refuses it early with a clear message.
# The receiver's Inbox is moved onto a 4 MiB tmpfs for the test and restored on every exit path.
#   tools/qualify-full-disk.sh --receiver anvit@hp --sender-command "<command that runs a shell on the sender>" --receiver-name hp
# The receiver needs passwordless sudo (mount/umount of its own temporary directory only).
# The sender command receives a shell script on stdin, e.g. "ssh -o BatchMode=yes anvit@other bash -s".
set -eu
receiver='' sender='' name='' size_mib=20
while [ "$#" -gt 0 ]; do
 case "$1" in
  --receiver) receiver=$2; shift 2;; --sender-command) sender=$2; shift 2;;
  --receiver-name) name=$2; shift 2;; --size-mib) size_mib=$2; shift 2;;
  *) echo "unknown argument: $1" >&2; exit 2;;
 esac
done
[ -n "$receiver" ] && [ -n "$sender" ] && [ -n "$name" ] || { echo '--receiver, --sender-command and --receiver-name are required' >&2; exit 2; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
rx() { ssh -o BatchMode=yes "$receiver" "$1"; }
mounted=''
restore() {
 rx 'P=~/.local/bin/pb; $P inbox reset >/dev/null 2>&1; D=$HOME/.pb-full-disk-inbox; if mountpoint -q "$D"; then sudo -n umount "$D"; fi; rmdir "$D" 2>/dev/null; true' || true
}
trap restore EXIT HUP INT TERM

echo "== receiver Inbox on a 4 MiB tmpfs"
rx 'D=$HOME/.pb-full-disk-inbox; mkdir -p "$D" && sudo -n mount -t tmpfs -o size=4m,uid=$(id -u),gid=$(id -g) tmpfs "$D" && ~/.local/bin/pb inbox set "$D" >/dev/null' || fail "could not prepare the small Inbox"
mounted=1

echo "== sender sends $size_mib MiB"
out=$(printf '%s\n' "P=~/.local/bin/pb; head -c $((size_mib*1024*1024)) /dev/urandom > /tmp/pb-full-disk.bin; start=\$(date +%s); timeout 120 \$P send /tmp/pb-full-disk.bin --to $name --json; echo \"RC=\$? ELAPSED=\$((\$(date +%s)-start))\"; rm -f /tmp/pb-full-disk.bin" | sh -c "$sender" 2>&1) || true
echo "$out" | tail -3 | cut -c1-300
echo "$out" | grep -q '"code":"destination_storage_full"' || fail "send was not refused as destination_storage_full"
echo "$out" | grep -q '"state_changed":false' || fail "refusal does not say nothing changed"
echo "$out" | grep -Eq 'no room|MiB free' || fail "refusal does not say how much room there is"
elapsed=$(echo "$out" | sed -n 's/.*ELAPSED=\([0-9]*\).*/\1/p' | tail -1)
[ -n "$elapsed" ] && [ "$elapsed" -le 30 ] || fail "the refusal took ${elapsed:-unknown} s; it must be prompt"
echo "PASS full disk refused in ${elapsed} s"
