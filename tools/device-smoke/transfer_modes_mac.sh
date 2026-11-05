#!/bin/bash
# transfer_modes_mac.sh ALIAS  (F22-01, macOS twin of transfer_modes.sh)
#
# The Mac sends to ALIAS (a paired machine of the same account whose Inbox accepts automatically), with the same
# steps and pass conditions as transfer_modes.sh: automatic 120 MiB + a 600 MiB batch of six arrive byte-identical, a
# 2.2 GB send is refused before data moves; unlimited 300 MiB with the Mac's UDP blocked waits, then resumes
# byte-identical; path loss mid-send stops at once (Mac egress over 30 s < 3 MB), survives a daemon restart, resumes.
# MAC_CMD and TARGET_CMD take ONE shell-string argument (keychain unlocked on the Mac); sudo on the Mac reads its
# password from SUDO_PW_FILE there. The UDP block is a pf anchor with a 600 s failsafe. Prints one JSON line.
set -uo pipefail
ALIAS=${1:?alias}
M() { $MAC_CMD "SUDO_PW_FILE=${SUDO_PW_FILE:?path of the sudo password file on the Mac}; $1"; }
T() { $TARGET_CMD "$1"; }
ANCHOR=com.apple/pbxfer
block() { M "cat \$SUDO_PW_FILE | sudo -S -p '' sh -c \"printf 'block drop out quick proto udp from any to any port != 53\nblock drop out quick inet6 proto udp all\n' | pfctl -a $ANCHOR -f - 2>/dev/null; pfctl -E >/dev/null 2>&1; pfctl -F states >/dev/null 2>&1; pfctl -k 0.0.0.0/0 >/dev/null 2>&1; (sleep 600; pfctl -a $ANCHOR -F all >/dev/null 2>&1) </dev/null >/dev/null 2>&1 &\""; }
unblock() { M "cat \$SUDO_PW_FILE | sudo -S -p '' pfctl -a $ANCHOR -F all >/dev/null 2>&1; true"; }
tx() { M "netstat -ibn -I en0 | awk 'NR==2{print \$10}'"; }
# TCP bytes and UDP datagrams sent so far, so a leak names the protocol that carried it
proto() { M "cat \$SUDO_PW_FILE | sudo -S -p '' sh -c 'netstat -s -p tcp; netstat -s -p udp'" | python3 -c "import re,sys;t=sys.stdin.read();b=re.search(r'([0-9]+) data packets? \(([0-9]+) bytes?\)',t);d=re.search(r'([0-9]+) datagrams? output',t);print(b.group(2) if b else 0,d.group(1) if d else 0)"; }
arrived() { [ "$(T "sha256sum ~/Paperboat/Inbox/$1 2>/dev/null | cut -d' ' -f1")" = "$2" ]; }
pbs() { M "~/.local/bin/pb send $1 --to $ALIAS $2"; }
exited() { M "grep -h '^EXIT=' ~/x-$1.log 2>/dev/null"; }
R=()
finish() {
  unblock
  # an interrupted send leaves its transfers open on the target, holding its receive slots
  M "for t in \$(~/.local/bin/pb send list --on $ALIAS 2>/dev/null | awk '\$2==\"uploading\"||\$2==\"created\"{print \$1}'); do ~/.local/bin/pb send cancel \$t --on $ALIAS >/dev/null 2>&1; done"
  M "rm -rf ~/x-auto.bin ~/x-batch ~/x-huge.bin ~/x-unl.bin ~/x-*.log"
  T "cd ~/Paperboat/Inbox && rm -f x-auto.bin b?.bin x-unl.bin"
  python3 -c 'import json,sys;print(json.dumps({"check":"transfer-modes-mac","status":sys.argv[1],"steps":sys.argv[2].split(),"detail":sys.argv[3]}))' "$1" "${R[*]}" "$2"
  [ "$1" = PASS ]; exit $?
}
trap 'finish FAIL interrupted' INT TERM
T "cd ~/Paperboat/Inbox 2>/dev/null && rm -f x-auto.bin b?.bin x-unl.bin; true"

M "head -c 125829120 /dev/urandom > ~/x-auto.bin"
sha=$(M "shasum -a 256 ~/x-auto.bin | cut -d' ' -f1")
pbs '~/x-auto.bin' "> ~/x-auto.log 2>&1" && arrived x-auto.bin "$sha" && R+=(AUTOMATIC_OK) || R+=(AUTOMATIC_FAILED)

M "mkdir -p ~/x-batch && for i in 1 2 3 4 5 6; do head -c 104857600 /dev/urandom > ~/x-batch/b\$i.bin; done"
want=$(M "cd ~/x-batch && shasum -a 256 b*.bin | cut -d' ' -f1 | sort")
pbs "~/x-batch/b1.bin ~/x-batch/b2.bin ~/x-batch/b3.bin ~/x-batch/b4.bin ~/x-batch/b5.bin ~/x-batch/b6.bin" "> ~/x-batch.log 2>&1" \
  && [ "$(T "cd ~/Paperboat/Inbox && sha256sum b?.bin | cut -d' ' -f1 | sort")" = "$want" ] && R+=(BATCH_OK) || R+=(BATCH_FAILED)

M "mkfile -n 2200m ~/x-huge.bin"
if pbs '~/x-huge.bin' "> ~/x-huge.log 2>&1"; then R+=(HUGE_ACCEPTED)
elif M "grep -q 'Automatic sends up to' ~/x-huge.log" && ! T "test -e ~/Paperboat/Inbox/x-huge.bin"; then R+=(HUGE_REFUSED)
else R+=(HUGE_FAILED); fi

M "head -c 314572800 /dev/urandom > ~/x-unl.bin"
sha=$(M "shasum -a 256 ~/x-unl.bin | cut -d' ' -f1")
block; sleep 20 # the direct path ages out
M "(~/.local/bin/pb send ~/x-unl.bin --to $ALIAS --unlimited > ~/x-unl.log 2>&1; echo EXIT=\$? >> ~/x-unl.log) </dev/null >/dev/null 2>&1 &"
waited=
for i in $(seq 1 120); do
  [ -n "$(exited unl)" ] && break
  M "grep -qiE 'wait|direct|relay' ~/x-unl.log" && { waited=1; break; }
  sleep 1
done
sleep 10
[ -n "$waited" ] && ! arrived x-unl.bin "$sha" && R+=(UNLIMITED_PAUSED) || R+=(UNLIMITED_NOT_PAUSED)
unblock
for i in $(seq 1 900); do [ -n "$(exited unl)" ] && break; sleep 1; done
[ "$(exited unl)" = EXIT=0 ] && arrived x-unl.bin "$sha" && R+=(UNLIMITED_RESUMED) || R+=(UNLIMITED_FAILED)

T "rm -f ~/Paperboat/Inbox/x-unl.bin"
M "(~/.local/bin/pb send ~/x-unl.bin --to $ALIAS --unlimited > ~/x-loss.log 2>&1; echo EXIT=\$? >> ~/x-loss.log) </dev/null >/dev/null 2>&1 &"
start=$(tx)
for i in $(seq 1 120); do [ $(( $(tx) - start )) -ge 60000000 ] && break; sleep 1; done
block; sleep 10 # bytes in flight drain
a=$(tx); p0=($(proto)); sleep 30; leaked=$(( $(tx) - a )); p1=($(proto))
leak_by="tcp$(( ${p1[0]:-0} - ${p0[0]:-0} ))B_udp$(( ${p1[1]:-0} - ${p0[1]:-0} ))dg"
[ $leaked -lt 3000000 ] && [ -z "$(exited loss)" ] && ! arrived x-unl.bin "$sha" && R+=(LOSS_STOPPED) || R+=(LOSS_LEAKED_${leaked}_$leak_by)
M "~/.local/bin/pb service restart >/dev/null 2>&1" # the paused send must survive its daemon restarting
unblock
for i in $(seq 1 900); do [ -n "$(exited loss)" ] && break; sleep 1; done
[ "$(exited loss)" = EXIT=0 ] && arrived x-unl.bin "$sha" && R+=(LOSS_RESUMED) || R+=(LOSS_FAILED)

for s in AUTOMATIC_OK BATCH_OK HUGE_REFUSED UNLIMITED_PAUSED UNLIMITED_RESUMED LOSS_STOPPED LOSS_RESUMED; do
  [[ " ${R[*]} " == *" $s "* ]] || finish FAIL "missing $s; $(M 'tail -c 300 ~/x-*.log 2>/dev/null' | tr '\n' ' ')"
done
finish PASS "automatic 120 MiB + 600 MiB batch byte-identical, 2.2 GB refused; unlimited waited under the UDP block and resumed; path loss leaked $leaked B in 30 s, survived a restart, resumed"
