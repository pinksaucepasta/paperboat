#!/bin/bash
# multi_account_host.sh HOST BINARY MINT_B  (F23-01)
#
# Two OS users on one Linux host, each paired to its own account, use machines of the same name at the same time.
# MINT_B is a command on HOST that runs `pb` signed in to a second account (e.g.
# 'sudo -n docker exec -u pb CT /home/pb/.local/bin/pb'); HOST's own pb is the first account (fresh_pair_lib.sh).
# Fresh throwaway containers: target TA in account A and TB in account B, both named N, each serving its own
# marker on 127.0.0.1:PORT; host H with user pbt paired to A and user pbu2 paired to B, machine guard installed.
# Passes when, through the one host gateway (https://PORT.N.<domain>), pbt reaches TA's marker and pbu2 reaches
# TB's, and pbu2 asking for pbt's name gets no A marker. Prints one JSON line {"check":"multi-account-host",...};
# machines are revoked by exact id (A here; B ids are printed for the second account's cleanup) and containers
# removed on every exit path.
set -uo pipefail
HOST=${1:?host} BIN=${2:?binary} MINT_B=${3:?second-account pb command on host}
HERE=$(cd "$(dirname "$0")" && pwd)
STAMP=$(date +%s)
TA=mah-ta-$STAMP TB=mah-tb-$STAMP H=mah-h-$STAMP N=lanea-mah-$STAMP PORT=18480
IDS_A= IDS_B=

finish() {
  local status=$1 detail=$2 id
  ssh "$HOST" "sudo -n docker rm -f $TA $TB $H >/dev/null 2>&1; rm -f /tmp/$H-pb" || true
  for id in $IDS_A; do
    python3 "$HERE/cleanup_test_account.py" --id "$id" --stale '0 seconds' --apply >/dev/null 2>&1 || detail="$detail; revoke of $id failed"
  done
  python3 -c 'import json,sys;print(json.dumps({"check":"multi-account-host","status":sys.argv[1],"machine_ids":sys.argv[2].split(),"second_account_machine_ids":sys.argv[3].split(),"detail":sys.argv[4]}))' "$status" "$IDS_A" "$IDS_B" "$detail"
  [ "$status" = PASS ]
  exit $?
}

scp -q "$BIN" "$HOST:/tmp/$H-pb" || finish FAIL "could not copy the build to $HOST"
out=$( { echo "FRESH_BINARY=/tmp/$H-pb"; cat "$HERE/fresh_pair_lib.sh"; cat <<EOF; } | ssh "$HOST" "bash -s" 2>&1
set -euo pipefail
as() { local ct=\$1 user=\$2 uid=\$3; shift 3; sudo -n docker exec -u \$user -e HOME=/home/\$user -e XDG_RUNTIME_DIR=/run/user/\$uid \$ct "\$@"; }
origin() { # origin CONTAINER MARKER
  as \$1 pbt 1000 bash -c "mkdir -p ~/origin && echo \$2 > ~/origin/marker && cd ~/origin && nohup python3 -m http.server $PORT --bind 127.0.0.1 >/dev/null 2>&1 &"
}
fresh $TA $N | sed 's/^MACHINE_ID=/A_MACHINE_ID=/'
FRESH_MINT="$MINT_B" fresh $TB $N | sed 's/^MACHINE_ID=/B_MACHINE_ID=/'
origin $TA MARKER_A_$STAMP
origin $TB MARKER_B_$STAMP
start_ct $H
pair_user $H pbt 1000 $N-ha | sed 's/^MACHINE_ID=/A_MACHINE_ID=/'
add_user $H pbu2 1001
FRESH_MINT="$MINT_B" pair_user $H pbu2 1001 $N-hb | sed 's/^MACHINE_ID=/B_MACHINE_ID=/'
as $H pbt 1000 /home/pbt/.local/bin/pb daemon machine-guard install >/dev/null 2>&1 || as $H pbt 1000 sudo -n /home/pbt/.local/bin/pb daemon machine-guard install
domain() { as $H \$1 \$2 /home/\$1/.local/bin/pb config local-access show --json | python3 -c 'import json,sys;d=json.load(sys.stdin)["data"];print(d.get("domain") or d["local_access"]["domain"])'; }
DA=\$(domain pbt 1000); DB=\$(domain pbu2 1001)
fetch() { # fetch USER UID HOSTNAME: body of the marker through the gateway, empty on failure
  as $H \$1 \$2 curl -sk --noproxy '*' --connect-timeout 10 --max-time 20 https://\$3/marker 2>/dev/null || true
}
for i in \$(seq 1 60); do
  a=\$(fetch pbt 1000 $PORT.$N.\$DA); b=\$(fetch pbu2 1001 $PORT.$N.\$DB)
  [ "\$a" = MARKER_A_$STAMP ] && [ "\$b" = MARKER_B_$STAMP ] && break
  sleep 3
done
echo "A_GOT=\$a"; echo "B_GOT=\$b"
[ "\$a" = MARKER_A_$STAMP ] && echo USER_A_OK
[ "\$b" = MARKER_B_$STAMP ] && echo USER_B_OK
cross=\$(fetch pbu2 1001 $PORT.$N.\$DA)
[ "\$cross" != MARKER_A_$STAMP ] && echo ISOLATED
EOF
)
IDS_A=$(sed -n 's/^A_MACHINE_ID=\(machine_[0-9a-f-]*\)$/\1/p' <<<"$out" | tr '\n' ' ')
IDS_B=$(sed -n 's/^B_MACHINE_ID=\(machine_[0-9a-f-]*\)$/\1/p' <<<"$out" | tr '\n' ' ')
if grep -qx USER_A_OK <<<"$out" && grep -qx USER_B_OK <<<"$out" && grep -qx ISOLATED <<<"$out"; then
  finish PASS "two OS users on one host, two accounts, same machine name: each reached its own account's machine through the one gateway; the second user could not reach the first's"
fi
finish FAIL "$(tail -c 600 <<<"$out" | tr '\n' ' ')"
