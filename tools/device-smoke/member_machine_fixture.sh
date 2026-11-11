#!/bin/bash
# member_machine_fixture.sh HOST NAME PBBIN: a disposable systemd container NAME on HOST (image gap5-systemd, pbvm4) with PBBIN
# installed and paired as a machine of the SECOND account (the member of the shared test team): the member side of the
# two-account team checks (team_two_account.py --member-cmd, tunnel/preview ip-rules observers).
# The member is the console session STATE (mode 600); its API token is minted from it, lives one day and goes straight
# into the container's `pb login --api-token-file -`; the enrollment token is piped the same way. Nothing secret is printed.
# env: ORIGIN, STATE, PW_CHROMIUM, PW_MODULE (as mint_api_token.mjs). Prints one JSON line {"name","machine_id"}.
# Teardown: ssh HOST sudo docker rm -f NAME, then cleanup_test_account.py --prefix <name prefix> --id <machine_id> --apply.
set -euo pipefail
host=$1 name=$2 bin=$3
here=$(cd "$(dirname "$0")" && pwd)
ct() { ssh -n "$host" "sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 -w /home/pbt $name $*"; }
ssh -n "$host" "sudo -n docker rm -f $name >/dev/null 2>&1 || true
sudo -n docker run -d --name $name --hostname $name --privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock gap5-systemd >/dev/null
sleep 8
sudo -n docker exec $name loginctl enable-linger pbt
sudo -n docker exec $name systemctl start user@1000.service"
scp -q "$bin" "$host:/tmp/$name.pb"
ssh -n "$host" "sudo -n docker cp /tmp/$name.pb $name:/home/pbt/pb && rm -f /tmp/$name.pb && sudo -n docker exec $name sh -c 'chown pbt:pbt /home/pbt/pb && chmod 755 /home/pbt/pb'"
ct ./pb install --json >/dev/null
pbx=/home/pbt/.local/bin/pb
# a systemd container has no keyring: the member's session credentials go to a file (teammate_login.py copies this setting)
shown=$(ct "$pbx config show --json")
echo "$shown" | python3 -c 'import json,sys; d=json.load(sys.stdin); d=d.get("data",d); print(json.dumps({"server_url": d["server_url"], "auth": {"allow_file_fallback": True}}))' |
  ssh "$host" "sudo -n docker exec -i -u pbt $name sh -c 'mkdir -p /home/pbt/.config/paperboat && cat > /home/pbt/.config/paperboat/config.json'"
# the member's CLI session is the controlled teammate profile (teammate_signin.py), apart from the device's own identity;
# /home/pbt/pbm runs pb as that profile (team_two_account.py --member-pb)
python3 "$here/teammate_signin.py" "$host sudo -n" "$name" pbt >/dev/null
root=/home/pbt/.local/state/paperboat/device-smoke/teammate
printf '#!/bin/sh\nexport HOME=%s/home XDG_CONFIG_HOME=%s/config XDG_DATA_HOME=%s/data XDG_STATE_HOME=%s/state XDG_CACHE_HOME=%s/cache XDG_RUNTIME_DIR=/run/user/1000\nexec %s --config %s/config.json "$@"\n' \
  "$root" "$root" "$root" "$root" "$root" "$pbx" "$root" |
  ssh "$host" "sudo -n docker exec -i -u pbt $name sh -c 'cat > /home/pbt/pbm; chmod 755 /home/pbt/pbm'"
ct "/home/pbt/pbm machine add --json" | python3 -c '
import json, re, sys, urllib.request
command = json.load(sys.stdin)["data"]["linux_macos_command"]
script = urllib.request.urlopen(re.search(r"\x27(https?://[^\x27]+)\x27", command).group(1), timeout=60).read().decode()
sys.stdout.write(re.search(r"PAPERBOAT_ENROLLMENT_TOKEN=\x27([A-Za-z0-9]+)\x27", script).group(1))
' | ssh "$host" "sudo -n docker exec -i -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 $name sh -c 'cat > /home/pbt/tok; chmod 600 /home/pbt/tok'"
ct "$pbx pair --name $name --enrollment-token-file /home/pbt/tok --json" | python3 -c 'import json,sys; d=json.load(sys.stdin); d=d.get("data",d); print(json.dumps({"name":"'"$name"'","machine_id":d["machine_id"]}))'
ct rm -f /home/pbt/tok
