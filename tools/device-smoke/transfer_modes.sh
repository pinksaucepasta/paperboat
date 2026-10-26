#!/bin/bash
# transfer_modes.sh HOST BINARY  (F22-01)
#
# Pairs two fresh throwaway containers on HOST with BINARY (fresh_pair_lib.sh) and checks both transfer modes end
# to end, client → target with the target's Inbox on automatic acceptance:
#   automatic: a 120 MiB send (over the old 50 MiB cap) and a 600 MiB batch of six files arrive byte-identical;
#     a send over the 2 GB Automatic bound is refused before any data moves;
#   unlimited: a 300 MiB `pb send --unlimited` started with the client's UDP blocked (no direct path, so no eligible
#     path) says it is waiting and delivers nothing; after UDP is unblocked it resumes and arrives byte-identical;
#   path loss: the same send with UDP blocked mid-transfer stops at once (the client's egress over 30 s stays under
#     3 MB: nothing crosses a hosted relay), then resumes and arrives byte-identical.
# Prints one JSON line {"check":"transfer-modes","status":"PASS"|"FAIL",...}; machines are revoked by exact id and
# containers removed on every exit path.
set -uo pipefail
HOST=${1:?host} BIN=${2:?binary}
HERE=$(cd "$(dirname "$0")" && pwd)
STAMP=$(date +%s)
TARGET=xfer-t-$STAMP CLIENT=xfer-c-$STAMP ALIAS=lanea-xfer-$STAMP
MACHINE_IDS=

finish() {
  local status=$1 detail=$2 id
  ssh "$HOST" "sudo -n docker rm -f $TARGET $CLIENT >/dev/null 2>&1; rm -f /tmp/$TARGET-pb" || true
  for id in $MACHINE_IDS; do
    python3 "$HERE/cleanup_test_account.py" --id "$id" --stale '0 seconds' --apply >/dev/null 2>&1 || detail="$detail; revoke of $id failed"
  done
  python3 -c 'import json,sys;print(json.dumps({"check":"transfer-modes","status":sys.argv[1],"machine_ids":sys.argv[2].split(),"detail":sys.argv[3]}))' "$status" "$MACHINE_IDS" "$detail"
  [ "$status" = PASS ]
  exit $?
}

scp -q "$BIN" "$HOST:/tmp/$TARGET-pb" || finish FAIL "could not copy the build to $HOST"
out=$( { echo "FRESH_BINARY=/tmp/$TARGET-pb"; cat "$HERE/fresh_pair_lib.sh"; cat <<EOF; } | ssh "$HOST" "bash -s" 2>&1
set -euo pipefail
fresh $TARGET $ALIAS
fresh $CLIENT $ALIAS-c
T="sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 $TARGET"
C="sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 $CLIENT"
\$T /home/pbt/.local/bin/pb inbox policy automatic >/dev/null
arrived() { # arrived NAME SHA: the target's Inbox holds NAME with SHA
  \$T bash -c "sha256sum ~/Paperboat/Inbox/\$1 2>/dev/null | cut -d' ' -f1" | grep -qx "\$2"
}
\$C bash -c 'head -c 125829120 /dev/urandom > ~/auto.bin'
sha=\$(\$C bash -c 'sha256sum ~/auto.bin | cut -d" " -f1')
if \$C timeout 900 /home/pbt/.local/bin/pb send /home/pbt/auto.bin --to $ALIAS > /tmp/$CLIENT-auto.log 2>&1 && arrived auto.bin \$sha; then
  echo AUTOMATIC_OK
else
  echo AUTOMATIC_FAILED; tail -c 400 /tmp/$CLIENT-auto.log
fi
rm -f /tmp/$CLIENT-auto.log
\$C bash -c 'mkdir ~/batch; for i in 1 2 3 4 5 6; do head -c 104857600 /dev/urandom > ~/batch/b\$i.bin; done; cd ~/batch && sha256sum b*.bin | cut -d" " -f1 | sort > ~/batch.sha'
if \$C timeout 900 /home/pbt/.local/bin/pb send /home/pbt/batch/b1.bin /home/pbt/batch/b2.bin /home/pbt/batch/b3.bin /home/pbt/batch/b4.bin /home/pbt/batch/b5.bin /home/pbt/batch/b6.bin --to $ALIAS > /tmp/$CLIENT-batch.log 2>&1 \
  && [ "\$(\$T bash -c 'cd ~/Paperboat/Inbox && sha256sum b?.bin | cut -d" " -f1 | sort')" = "\$(\$C cat /home/pbt/batch.sha)" ]; then
  echo BATCH_OK
else
  echo BATCH_FAILED; tail -c 400 /tmp/$CLIENT-batch.log
fi
rm -f /tmp/$CLIENT-batch.log
\$C truncate -s 2200M /home/pbt/huge.bin
if \$C timeout 120 /home/pbt/.local/bin/pb send /home/pbt/huge.bin --to $ALIAS > /tmp/$CLIENT-huge.log 2>&1; then
  echo HUGE_ACCEPTED
elif grep -q 'Automatic sends up to' /tmp/$CLIENT-huge.log && ! \$T bash -c 'test -e ~/Paperboat/Inbox/huge.bin'; then
  echo HUGE_REFUSED
else
  echo HUGE_FAILED; tail -c 400 /tmp/$CLIENT-huge.log
fi
rm -f /tmp/$CLIENT-huge.log
\$C rm -f /home/pbt/huge.bin
\$C bash -c 'head -c 314572800 /dev/urandom > ~/unlimited.bin'
sha=\$(\$C bash -c 'sha256sum ~/unlimited.bin | cut -d" " -f1')
block="OUTPUT -p udp ! --dport 53 -j DROP"
sudo -n docker exec $CLIENT iptables -I \$block
sudo -n docker exec $CLIENT ip6tables -I \$block 2>/dev/null || true
sleep 20 # the direct path ages out
sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 -d $CLIENT bash -c 'timeout 1500 ~/.local/bin/pb send ~/unlimited.bin --to $ALIAS --unlimited > ~/unlimited.log 2>&1; echo EXIT=\$? >> ~/unlimited.log'
waited=
for i in \$(seq 1 120); do
  if \$C grep -qiE 'wait|direct|relay' /home/pbt/unlimited.log 2>/dev/null && ! \$C grep -q '^EXIT=' /home/pbt/unlimited.log; then waited=1; break; fi
  \$C grep -q '^EXIT=' /home/pbt/unlimited.log 2>/dev/null && break
  sleep 1
done
sleep 10
if [ -n "\$waited" ] && ! arrived unlimited.bin \$sha; then echo UNLIMITED_PAUSED; else echo UNLIMITED_NOT_PAUSED; \$C tail -c 300 /home/pbt/unlimited.log; fi
sudo -n docker exec $CLIENT iptables -D \$block
sudo -n docker exec $CLIENT ip6tables -D \$block 2>/dev/null || true
for i in \$(seq 1 900); do
  \$C grep -q '^EXIT=' /home/pbt/unlimited.log 2>/dev/null && break
  sleep 1
done
if \$C grep -qx 'EXIT=0' /home/pbt/unlimited.log && arrived unlimited.bin \$sha; then
  echo UNLIMITED_RESUMED
else
  echo UNLIMITED_FAILED; \$C tail -c 400 /home/pbt/unlimited.log
fi
tx() { sudo -n docker exec $CLIENT cat /sys/class/net/eth0/statistics/tx_bytes; }
\$T bash -c 'rm -f ~/Paperboat/Inbox/unlimited.bin'
sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 -d $CLIENT bash -c 'timeout 1500 ~/.local/bin/pb send ~/unlimited.bin --to $ALIAS --unlimited > ~/loss.log 2>&1; echo EXIT=\$? >> ~/loss.log'
start=\$(tx)
for i in \$(seq 1 120); do [ \$((\$(tx)-start)) -ge 60000000 ] && break; sleep 1; done
sudo -n docker exec $CLIENT iptables -I \$block
sudo -n docker exec $CLIENT ip6tables -I \$block 2>/dev/null || true
sleep 10 # bytes in flight drain
a=\$(tx); sleep 30; leaked=\$((\$(tx)-a))
if [ \$leaked -lt 3000000 ] && ! \$C grep -q '^EXIT=' /home/pbt/loss.log && ! arrived unlimited.bin \$sha; then echo LOSS_STOPPED; else echo LOSS_LEAKED \$leaked; \$C tail -c 300 /home/pbt/loss.log; fi
sudo -n docker exec $CLIENT iptables -D \$block
sudo -n docker exec $CLIENT ip6tables -D \$block 2>/dev/null || true
for i in \$(seq 1 900); do
  \$C grep -q '^EXIT=' /home/pbt/loss.log 2>/dev/null && break
  sleep 1
done
if \$C grep -qx 'EXIT=0' /home/pbt/loss.log && arrived unlimited.bin \$sha; then echo LOSS_RESUMED; else echo LOSS_FAILED; \$C tail -c 400 /home/pbt/loss.log; fi
EOF
)
MACHINE_IDS=$(sed -n 's/^MACHINE_ID=\(machine_[0-9a-f-]*\)$/\1/p' <<<"$out" | tr '\n' ' ')
if grep -qx AUTOMATIC_OK <<<"$out" && grep -qx BATCH_OK <<<"$out" && grep -qx HUGE_REFUSED <<<"$out" && grep -qx UNLIMITED_PAUSED <<<"$out" && grep -qx UNLIMITED_RESUMED <<<"$out" && grep -qx LOSS_STOPPED <<<"$out" && grep -qx LOSS_RESUMED <<<"$out"; then
  finish PASS "automatic 120 MiB and a 600 MiB batch delivered byte-identical, 2.2 GB refused; unlimited 300 MiB waited with UDP blocked, resumed byte-identical, and stopped at once when UDP was lost mid-transfer"
fi
finish FAIL "$(tail -c 600 <<<"$out" | tr '\n' ' ')"
