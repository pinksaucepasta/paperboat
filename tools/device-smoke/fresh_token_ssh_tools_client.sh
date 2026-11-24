#!/bin/bash
# Runs inside the FRESH client container (enrolled from a token only: no login, no ssh key, no agent) as the paired
# user. Env: ALIAS (target alias), TARGET_ID (machine_<id>), STAMP. For each way of naming the target (alias, name,
# machine id) `pb ssh`, `pb scp` and `pb rsync` must work and move the exact bytes: a file goes up, its hash is
# computed ON THE TARGET by pb ssh, and a copy comes back byte-identical. Prints RESULT lines; exits 1 on any FAIL.
set -u
PB=~/.local/bin/pb
unset SSH_AUTH_SOCK
fail=0
note() { echo "RESULT $*"; }
# The client really has no ssh identity: a pass here must not come from a key or agent the harness left behind.
if ls ~/.ssh/id_* >/dev/null 2>&1 || ssh-add -l >/dev/null 2>&1; then note "FAIL client-has-ssh-identity"; exit 1; fi
NAME=$($PB machine list --all --json | python3 -c 'import json,sys;t=sys.argv[1];print(next(m.get("name") or m["alias"] for m in json.load(sys.stdin)["data"]["machines"] if m["id"]==t))' "$TARGET_ID") || NAME=$ALIAS
head -c 262144 /dev/urandom > ~/fts-src.bin
want=$(sha256sum < ~/fts-src.bin | cut -d' ' -f1)
for ref in "$ALIAS" "$NAME" "$TARGET_ID"; do
  tag=$(echo "$ref" | tr -c 'A-Za-z0-9\n' _)
  x=$((RANDOM+1000)); y=$((RANDOM+1000))
  # one command string: pb ssh joins its arguments for the remote shell, so `sh -c a b` would lose its quoting
  got=$(timeout 60 $PB ssh "$ref" -- "echo \$(($x+$y))" 2>&1 | tr -d '\r' | tail -n 1)
  [ "$got" = "$((x+y))" ] && note "PASS ssh $ref" || { note "FAIL ssh $ref: ${got:0:120}"; fail=1; }
  for tool in scp rsync; do
    dest=/tmp/fts-$tool-$tag.bin
    if [ $tool = scp ]; then
      timeout 90 $PB scp ~/fts-src.bin "$ref:$dest" >/tmp/fts.out 2>&1 && timeout 90 $PB scp "$ref:$dest" ~/fts-back.bin >>/tmp/fts.out 2>&1
    else
      timeout 90 $PB rsync -a ~/fts-src.bin "$ref:$dest" >/tmp/fts.out 2>&1 && timeout 90 $PB rsync -a "$ref:$dest" ~/fts-back.bin >>/tmp/fts.out 2>&1
    fi
    rc=$?
    remote=$(timeout 60 $PB ssh "$ref" -- sha256sum "$dest" 2>/dev/null | tr -d '\r' | cut -d' ' -f1 | tail -n 1)
    back=$(sha256sum < ~/fts-back.bin 2>/dev/null | cut -d' ' -f1)
    if [ $rc = 0 ] && [ "$remote" = "$want" ] && [ "$back" = "$want" ]; then note "PASS $tool $ref"
    else note "FAIL $tool $ref: rc=$rc remote=${remote:0:12} back=${back:0:12} $(tr '\n' ' ' </tmp/fts.out | tail -c 160)"; fail=1; fi
    rm -f ~/fts-back.bin
  done
  timeout 30 $PB ssh "$ref" -- rm -f /tmp/fts-scp-$tag.bin /tmp/fts-rsync-$tag.bin >/dev/null 2>&1
done
rm -f ~/fts-src.bin /tmp/fts.out
exit $fail
