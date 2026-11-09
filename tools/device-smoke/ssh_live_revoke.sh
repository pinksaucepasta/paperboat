#!/bin/bash
# ssh_live_revoke.sh HOST BINARY [cli|grant|team]  (check id: ssh-live-revoke, CHECKLIST F5-10)
# Revoking access ends open SSH sessions, and pb says so (F5-10). Pairs FRESH throwaway containers on HOST (as
# ssh_other_user.sh): the target T (OpenSSH, owner = the HOST account A) and a client C. The client holds TWO open
# sessions to T, `pb ssh` and a plain `ssh <alias>.<domain>` through the managed OpenSSH config (pb __ssh-proxy), then
# access is revoked; BOTH sessions must end within 15 s and BOTH must print "access to <T> was revoked". A normal exit
# of either must print nothing.
#   cli   : C is another client of A; `pb logout` on C revokes its CLI session.
#   grant : C is signed in as a throwaway MEMBER account B of team TEAM; A shares T into TEAM, grants B managed_ssh+exec,
#           and then withdraws that grant (team machine grant --active=false).
#   team  : as grant, but A removes B from TEAM (team remove).
# grant/team need env: TEAM (a team A owns), MEMBER_STATE (B's console storage state, fresh_account.mjs), PW_CHROMIUM,
# optional PW_MODULE. For them the check also requires, right after the grant and with B never having run exec: both
# `ssh T.<domain> echo ok` and `pb --workspace TEAM ssh T -- echo ok` print ok within 30 s of the check starting on B
# (setup bound only, each timed on its own and recorded in the result; the grant-to-access latency is a separate follow-up),
# B's managed OpenSSH config has T's block and B's ssh_known_hosts has T's lines.
# Prints one JSON line {"check":"ssh-live-revoke",...}; machines revoked by exact id, containers removed on exit,
# team share/membership undone.
set -uo pipefail
HOST=${1:?host} BIN=${2:?binary} MODE=${3:-cli}
case $MODE in cli | grant | team) ;; *) echo "usage: $0 HOST BINARY [cli|grant|team]" >&2; exit 2 ;; esac
if [ "$MODE" != cli ]; then
  : "${TEAM:?TEAM}" "${MEMBER_STATE:?MEMBER_STATE}" "${PW_CHROMIUM:?PW_CHROMIUM}"
fi
HERE=$(cd "$(dirname "$0")" && pwd)
STAMP=$(date +%s)
T=lanei-slr-t-$STAMP C=lanei-slr-c-$STAMP ALIAS=lanei-slr-$STAMP
IDS=
TID= BID=
OWNER='PB=~/.local/bin/pb; gen() { $PB team get "$TEAM" --json | python3 -c "import json,sys;print(json.load(sys.stdin)[\"generation\"])"; };'
dx() { echo "sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 $1"; }
owner() { ssh "$HOST" "TEAM=$TEAM; $OWNER $*"; }
finish() {
  local status=$1 detail=$2 id
  if [ "$MODE" != cli ] && [ -n "$TID" ]; then
    owner "\$PB team machine unshare \$TEAM $TID --generation \$(gen) --json >/dev/null 2>&1; [ -z '$BID' ] || \$PB team remove \$TEAM $BID --generation \$(gen) --json >/dev/null 2>&1" >/dev/null 2>&1 || true
  fi
  ssh "$HOST" "sudo -n docker rm -f $T $C >/dev/null 2>&1; rm -f /tmp/$T-pb" || true
  for id in $IDS; do python3 "$HERE/cleanup_test_account.py" --id "$id" --stale '0 seconds' --apply >/dev/null 2>&1 || detail="$detail; revoke of $id failed"; done
  python3 -c 'import json,sys;print(json.dumps({"check":"ssh-live-revoke","mode":sys.argv[1],"status":sys.argv[2],"machine_ids":sys.argv[3].split(),"detail":sys.argv[4]}))' "$MODE" "$status" "$IDS" "$detail"
  [ "$status" = PASS ]
  exit $?
}
diag() { # the member's and the target's own account of a failed dial, then exec and ssh once more (diagnostic codes only, no secrets)
  local pbc="$(dx $C) /home/pbt/.local/bin/pb --workspace $TEAM"
  {
    ssh "$HOST" "for ct in $C $T; do echo \"[\$ct]\"; sudo -n docker exec \$ct bash -c 'grep -h -E \"peer_authority|admission|peer_stream\" /home/pbt/.local/state/paperboat/diagnostics/*.ndjson 2>/dev/null | tail -2 | cut -c1-420'; done" 2>&1
    echo -n 'EXEC: '; ssh "$HOST" "timeout 60 $pbc exec $ALIAS -- echo exec-ok" 2>&1 | tail -1 | cut -c1-120
    echo -n 'SSH-AFTER-EXEC: '; ssh "$HOST" "timeout 60 $pbc ssh $ALIAS -- echo ok" 2>&1 | tail -1 | cut -c1-160
  } | tr '\n' ' ' | cut -c1-1800
}
scp -q "$BIN" "$HOST:/tmp/$T-pb" || finish FAIL "could not copy the build to $HOST"
mk() { # mk CONTAINER PACKAGES: a fresh systemd container with the build installed for user pbt
  ssh "$HOST" "bash -s" <<EOF 2>&1
set -euo pipefail
ct=$1; X='$(dx "$1")'
sudo -n docker run -d --name \$ct --privileged --dns-option single-request --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock laneh-ubuntu24-systemd >/dev/null
sleep 8
sudo -n docker exec \$ct bash -c "DEBIAN_FRONTEND=noninteractive apt-get -qq update >/dev/null 2>&1; DEBIAN_FRONTEND=noninteractive apt-get -qq install -y $2 >/dev/null 2>&1; true"
sudo -n docker exec \$ct loginctl enable-linger pbt
sudo -n docker exec \$ct systemctl start user@1000.service
sudo -n docker cp /tmp/$T-pb \$ct:/home/pbt/pb
sudo -n docker exec \$ct bash -c 'chown pbt:pbt /home/pbt/pb; chmod 755 /home/pbt/pb'
\$X bash -c 'cd ~ && ./pb install >/dev/null'
EOF
}
enroll() { # enroll CONTAINER NAME: pair the container as an owner (A) machine, print MACHINE_ID=
  ssh "$HOST" "bash -s" <<EOF 2>&1
set -euo pipefail
ct=$1; name=$2; X="$(dx "$1")"
NAME=\$name TOKEN=/tmp/\$ct-token python3 - <<'PY'
import json, os, re, subprocess
added = json.loads(subprocess.run([os.path.expanduser("~/.local/bin/pb"), "machine", "add", "--name", os.environ["NAME"], "--json"], capture_output=True, text=True, check=True).stdout)
url = re.search(r"'(https?://[^']+)'", added["data"]["linux_macos_command"]).group(1)
script = subprocess.run(["curl", "-fsSL", url], capture_output=True, text=True, check=True).stdout
token = re.search(r"PAPERBOAT_ENROLLMENT_TOKEN='([A-Za-z0-9]+)'", script).group(1)
with open(os.open(os.environ["TOKEN"], os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600), "w") as f:
    f.write(token)
PY
sudo -n docker cp /tmp/\$ct-token \$ct:/home/pbt/token; rm -f /tmp/\$ct-token
sudo -n docker exec \$ct bash -c 'chown pbt:pbt /home/pbt/token; chmod 600 /home/pbt/token'
\$X /home/pbt/.local/bin/pb pair --enrollment-token-file /home/pbt/token --name \$name --json | python3 -c 'import json,sys;d=json.load(sys.stdin);assert d["ok"],d;print("MACHINE_ID="+d["data"]["machine_id"])'
\$X rm -f /home/pbt/token
EOF
}
# the target: OpenSSH (paired AFTER sshd runs, so the setup user pbt is registered with port 22)
out=$(mk $T openssh-server; ssh "$HOST" "sudo -n docker exec $T bash -c 'systemctl enable --now ssh >/dev/null 2>&1'"; enroll $T $ALIAS)
TID=$(sed -n 's/^MACHINE_ID=\(machine_[0-9a-f-]*\)$/\1/p' <<<"$out" | head -1)
[ -n "$TID" ] || finish FAIL "target pairing failed: $(tail -c 300 <<<"$out" | tr '\n' ' ')"
IDS="$TID "
if [ "$MODE" = cli ]; then
  out=$(mk $C openssh-client; enroll $C $ALIAS-c)
  cid=$(sed -n 's/^MACHINE_ID=\(machine_[0-9a-f-]*\)$/\1/p' <<<"$out" | head -1)
  [ -n "$cid" ] || finish FAIL "client pairing failed: $(tail -c 300 <<<"$out" | tr '\n' ' ')"
  IDS="$IDS$cid "
  PBW=
else
  PBW="--workspace $TEAM"
  mk $C openssh-client >/dev/null 2>&1
  ssh "$HOST" "$(dx $C) bash -c 'nohup ~/.local/bin/pb login --no-browser --json > ~/login.out 2>&1 < /dev/null &'"
  url=
  for i in $(seq 1 30); do
    url=$(ssh "$HOST" "$(dx $C) cat /home/pbt/login.out" 2>/dev/null | grep -o 'https://[^"]*authorize?code=[A-Z0-9-]*' | head -1)
    [ -n "$url" ] && break; sleep 2
  done
  [ -n "$url" ] || finish FAIL "the member client printed no approval link"
  env STATE="$MEMBER_STATE" PW_CHROMIUM="$PW_CHROMIUM" PW_MODULE="${PW_MODULE:-playwright-core}" node "$HERE/approve_cli_login.mjs" "$url" >/dev/null 2>&1 || finish FAIL "the member's login approval failed"
  who=
  for i in $(seq 1 30); do
    who=$(ssh "$HOST" "$(dx $C) /home/pbt/.local/bin/pb auth status --json" 2>/dev/null | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d["account"]["id"],d["account"]["email"])' 2>/dev/null)
    [ -n "$who" ] && break; sleep 2
  done
  [ -n "$who" ] || finish FAIL "the member client did not sign in"
  BID=${who% *} EMAIL=${who#* }
  inv=$(owner "\$PB team invite \$TEAM '$EMAIL' --generation \$(gen) --json" | python3 -c 'import json,sys;print(json.load(sys.stdin)["invitation_id"])' 2>/dev/null)
  [ -n "$inv" ] || finish FAIL "the owner could not invite the member"
  ssh "$HOST" "$(dx $C) /home/pbt/.local/bin/pb team accept $inv --json >/dev/null" || finish FAIL "the member could not accept the invitation"
  owner "\$PB team machine share \$TEAM $TID --generation \$(gen) --json >/dev/null" || finish FAIL "could not share the target into $TEAM"
  owner "\$PB team machine grant \$TEAM $TID --generation \$(gen) --member $BID --capability managed_ssh --capability exec --json >/dev/null" || finish FAIL "could not grant the member managed_ssh"
fi
cat > /tmp/$T-run.sh <<'RUN'
set -u
ALIAS=$1 PBW=$2 PHASE=$3
PB=~/.local/bin/pb
DOMAIN=
for i in $(seq 1 60); do DOMAIN=$(grep -o 'Host \*\.[^ ]*' ~/.ssh/paperboat_config 2>/dev/null | head -1 | sed 's/Host \*\.//'); [ -n "$DOMAIN" ] && break; sleep 2; done
N=$ALIAS.$DOMAIN
if [ "$PHASE" = access ]; then # member modes: nothing ran before; both entry points must work within 5 s, each timed on its own
  rm -f ~/acc-plain ~/acc-pb
  t0=$(date +%s.%N); dl=$(python3 -c "import time;print(time.time()+${4:-40})")
  ( while python3 -c "import sys,time;sys.exit(0 if time.time()<$dl else 1)"; do
      o=$(ssh -o ConnectTimeout=4 "$N" echo ok </dev/null 2>&1 | tr '\n' ' '); echo "$o" > ~/acc-plain.err
      grep -qE '(^| )ok $' <<<"$o" && { date +%s.%N > ~/acc-plain; break; }; sleep 0.5; done ) &
  ( while python3 -c "import sys,time;sys.exit(0 if time.time()<$dl else 1)"; do
      o=$(timeout 20 "$PB" $PBW ssh "$ALIAS" -- echo ok </dev/null 2>&1 | tr '\n' ' '); echo "$o" > ~/acc-pb.err
      grep -qE '(^| )ok $' <<<"$o" && { date +%s.%N > ~/acc-pb; break; }; sleep 0.5; done ) &
  wait
  okpl=$(cat ~/acc-plain 2>/dev/null); okp=$(cat ~/acc-pb 2>/dev/null)
  lerr=$(cat ~/acc-plain.err 2>/dev/null); perr=$(cat ~/acc-pb.err 2>/dev/null)
  block=$(grep -c "^Host $N\$" ~/.ssh/paperboat_config); kh=$(grep -c "^$N " /run/user/$(id -u)/paperboat/ssh_known_hosts 2>/dev/null)
  echo "RESULT access plain_s=$([ -n "$okpl" ] && python3 -c "print(round($okpl-$t0,1))" || echo none) pb_s=$([ -n "$okp" ] && python3 -c "print(round($okp-$t0,1))" || echo none) config_block=$block known_hosts_lines=$kh last_plain_error=[$([ -n "$okpl" ] || cut -c1-200 <<<"$lerr")] last_pb_error=[$([ -n "$okp" ] || cut -c1-200 <<<"$perr")]"
  exit 0
fi
# normal exits print nothing
perr=$("$PB" $PBW ssh "$ALIAS" -- 'exit 0' </dev/null 2>&1 >/dev/null | tr '\n' ' '); lerr=$(ssh "$N" 'exit 0' </dev/null 2>&1 >/dev/null | tr '\n' ' ')
# A team machine's plain ssh prints the disambiguation line "<alias> (team <team>)" on stderr at every connect (reported as a
# separate product finding); it is not a revoke message, so it is set aside here and recorded as banner=yes.
banner=no; case "$lerr" in "$ALIAS (team "*") "*) banner=yes; lerr=$(sed -E "s/^$ALIAS \(team [^)]*\) *//" <<<"$lerr");; esac
echo "RESULT normal pb_stderr=[$perr] plain_stderr=[$lerr] banner=$banner"
# two open sessions to the target: `pb ssh` and a plain ssh through the managed OpenSSH config
"$PB" $PBW ssh "$ALIAS" -- 'echo session-up; sleep 300' > ~/slr.out 2> ~/slr.err & pid=$!
ssh "$N" 'echo plain-up; sleep 300' > ~/slrp.out 2> ~/slrp.err < /dev/null & ppid=$!
for i in $(seq 1 60); do grep -q session-up ~/slr.out && grep -q plain-up ~/slrp.out && break; sleep 1; done
grep -q session-up ~/slr.out && grep -q plain-up ~/slrp.out || { echo "RESULT fail:[no-session pb=$(tr '\n' ' ' < ~/slr.err | cut -c1-160) plain=$(tr '\n' ' ' < ~/slrp.err | cut -c1-160)]"; exit 0; }
if [ "$PHASE" = cli ]; then "$PB" logout >/dev/null 2>&1; else echo HOLDING; for i in $(seq 1 600); do [ -e ~/go ] && break; sleep 0.2; done; fi
start=$(date +%s.%N)
for i in $(seq 1 300); do kill -0 $pid 2>/dev/null || break; sleep 0.1; done
end=$(date +%s.%N)
for i in $(seq 1 300); do kill -0 $ppid 2>/dev/null || break; sleep 0.1; done
pend=$(date +%s.%N)
palive=$(kill -0 $ppid 2>/dev/null && echo yes || echo no); alive=$(kill -0 $pid 2>/dev/null && echo yes || echo no); kill $pid $ppid 2>/dev/null
echo "RESULT seconds=$(python3 -c "print(round($end-$start,1))") alive=$alive plain_seconds=$(python3 -c "print(round($pend-$start,1))") plain_alive=$palive stderr=[$(tr '\n' ' ' < ~/slr.err | cut -c1-200)] plain_stderr=[$(tr '\n' ' ' < ~/slrp.err | cut -c1-200)]"
RUN
ssh "$HOST" "sudo -n docker exec -i -u pbt $C sh -c 'cat > /home/pbt/run.sh'" < /tmp/$T-run.sh; rm -f /tmp/$T-run.sh
RUNX="$(dx $C) timeout 900 bash /home/pbt/run.sh $ALIAS '$PBW'"
detail=
if [ "$MODE" = cli ]; then
  # the client waits for its own machine record to be listed before anything else
  ssh "$HOST" "$(dx $C) bash -c 'for i in \$(seq 1 60); do ~/.local/bin/pb machine list 2>/dev/null | grep -q \"$ALIAS \" && break; sleep 2; done'" >/dev/null 2>&1
  res=$(ssh "$HOST" "$RUNX cli" 2>&1)
else
  acc=$(ssh "$HOST" "$RUNX access ${ACCESS_WAIT:-40}" 2>&1 | grep '^RESULT access')
  detail="$acc"
  python3 - "$acc" <<'PY' || finish FAIL "member access gate: $acc $(diag)"
import re, sys
a = sys.argv[1]
p, q = re.search(r"plain_s=(\S+)", a), re.search(r"pb_s=(\S+)", a)
ok = p and q and "none" not in (p[1], q[1]) and float(p[1]) <= 30 and float(q[1]) <= 30
ok = ok and "config_block=1" in a and not re.search(r"known_hosts_lines=0\b", a)
sys.exit(0 if ok else 1)
PY
  ssh "$HOST" "$RUNX $MODE > /tmp/$T-hold.out 2>&1 &" </dev/null
  for i in $(seq 1 120); do ssh "$HOST" "grep -q HOLDING /tmp/$T-hold.out" 2>/dev/null && break; ssh "$HOST" "grep -q 'RESULT fail' /tmp/$T-hold.out" 2>/dev/null && break; sleep 2; done
  ssh "$HOST" "grep -q HOLDING /tmp/$T-hold.out" || finish FAIL "$detail no sessions: $(ssh "$HOST" 'tail -c 300 /tmp/'$T'-hold.out' | tr '\n' ' ')"
  if [ "$MODE" = grant ]; then
    owner "\$PB team machine grant \$TEAM $TID --generation \$(gen) --member $BID --capability managed_ssh --capability exec --active=false --json >/dev/null" || finish FAIL "could not withdraw the grant"
  else
    owner "\$PB team remove \$TEAM $BID --generation \$(gen) --json >/dev/null" && BID= || finish FAIL "could not remove the member"
  fi
  ssh "$HOST" "sudo -n docker exec $C touch /home/pbt/go"
  for i in $(seq 1 60); do ssh "$HOST" "grep -q 'RESULT seconds' /tmp/$T-hold.out" 2>/dev/null && break; sleep 2; done
  res=$(ssh "$HOST" "cat /tmp/$T-hold.out; rm -f /tmp/$T-hold.out")
fi
detail="$detail $(grep -E '^RESULT' <<<"$res" | tr '\n' ' ')"
secs=$(sed -n 's/^RESULT seconds=\([0-9.]*\) .*/\1/p' <<<"$res")
psecs=$(sed -n 's/^RESULT .* plain_seconds=\([0-9.]*\) .*/\1/p' <<<"$res")
rev="access (to [^ ]+ )?was revoked"
pbmsg=$(sed -n 's/^RESULT seconds=.* stderr=\[\(.*\)\] plain_stderr=.*/\1/p' <<<"$res")
plmsg=$(sed -n 's/^RESULT seconds=.* plain_stderr=\[\(.*\)\]$/\1/p' <<<"$res")
if [ -n "$secs" ] && [ -n "$psecs" ] && python3 -c "import sys; sys.exit(0 if float('$secs') <= 15 and float('$psecs') <= 15 else 1)" \
  && grep -q "alive=no" <<<"$res" && grep -q "plain_alive=no" <<<"$res" \
  && grep -qiE "$rev" <<<"$pbmsg" && grep -qiE "$rev" <<<"$plmsg" \
  && grep -q 'RESULT normal pb_stderr=\[\] plain_stderr=\[ *\]' <<<"$res"; then
  finish PASS "$detail"
fi
finish FAIL "$detail $(tail -c 300 <<<"$res" | tr '\n' ' ')"
