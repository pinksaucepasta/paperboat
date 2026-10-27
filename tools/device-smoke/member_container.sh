#!/bin/bash
# Creates a throwaway hp container NAME paired to the member account (Playwright storage state in STATE) and reachable by
# `ssh NAME` (ssh config entry with ProxyJump hp is the caller's). usage: member_container.sh HOST NAME BUILD_PATH_ON_HOST
# env: ORIGIN STATE PW_CHROMIUM [PW_MODULE]. Remove it with `ssh HOST docker rm -f NAME` and revoke its machine record by exact id.
set -euo pipefail
host=$1 name=$2 build=$3 here=$(cd "$(dirname "$0")" && pwd)
ssh -n "$host" "docker rm -f $name >/dev/null 2>&1; ~/pbtest/mkct.sh $name $build" | head -1
node "$here/mint_enrollment_token.mjs" | ssh "$host" "docker exec -i $name bash -c 'cat > /home/tester/token; chown tester:tester /home/tester/token; chmod 600 /home/tester/token'"
ex="docker exec -u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001 -w /home/tester $name"
ssh -n "$host" "$ex /home/tester/pb install" 2>&1 | tail -1
# a freshly installed runtime needs a moment before it can pair ("host runtime command failed"): retry for up to ~2 minutes
for try in 1 2 3 4 5 6; do
  out=$(ssh -n "$host" "$ex /home/tester/.local/bin/pb pair --enrollment-token-file /home/tester/token --name $name --json" | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d.get("ok"), d.get("data",{}).get("machine_id"), d.get("error",{}).get("message"))')
  case "$out" in True*) break;; esac
  sleep 20
done
echo "$out"
ssh -n "$host" "docker exec -u tester $name rm -f /home/tester/token"
