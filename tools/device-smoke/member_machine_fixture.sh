#!/bin/bash
# member_machine_fixture.sh HOST NAME PBBIN: a disposable systemd container NAME on HOST (image gap5-systemd, Linux test VM) with PBBIN
# installed and paired as a machine of the SECOND account (the member of the shared test team): the member side of the
# two-account team checks (team_two_account.py --member-cmd, tunnel/preview ip-rules observers).
# The member is the console session STATE (mode 600); its API token is minted from it, lives one day and goes straight
# into the container's `pb login --api-token -`; the enrollment token is piped the same way. Nothing secret is printed.
# env: ORIGIN, STATE, PW_CHROMIUM, PW_MODULE (as mint_api_token.mjs). Prints one JSON line {"name","machine_id"}.
# Teardown: ssh HOST sudo docker rm -f NAME, then cleanup_test_account.py --prefix <name prefix> --id <machine_id> --apply.
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/ct_env.sh"
set -euo pipefail
. "$(cd "$(dirname "$0")" && pwd)/stage_binary.sh"
host=$1 name=$2 bin=$3
here=$(cd "$(dirname "$0")" && pwd)
ct() { ssh -n "$host" "$CT_DOCKER exec -u $CT_USER -e HOME=$CT_HOME -e XDG_RUNTIME_DIR=$CT_RUNDIR -w $CT_HOME $name $*"; }
ssh -n "$host" "$CT_DOCKER rm -f $name >/dev/null 2>&1 || true
$CT_DOCKER run -d --name $name --hostname $name --privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock $CT_IMAGE >/dev/null
sleep 8
$CT_DOCKER exec $name loginctl enable-linger $CT_USER
$CT_DOCKER exec $name systemctl start user@$CT_UID.service"
stage_binary "$bin" "$host" "/tmp/$name.pb"
ssh -n "$host" "$CT_DOCKER cp /tmp/$name.pb $name:$CT_HOME/pb && rm -f /tmp/$name.pb && $CT_DOCKER exec $name sh -c 'chown $CT_USER:$CT_USER $CT_HOME/pb && chmod 755 $CT_HOME/pb'"
ct ./pb install --json >/dev/null
pbx=$CT_HOME/.local/bin/pb
# a systemd container has no keyring: the member's session credentials go to a file (teammate_login.py copies this setting)
shown=$(ct "$pbx config show --json")
echo "$shown" | python3 -c 'import json,sys; d=json.load(sys.stdin); d=d.get("data",d); print(json.dumps({"server_url": d["server_url"], "auth": {"allow_file_fallback": True}}))' |
  ssh "$host" "$CT_DOCKER exec -i -u $CT_USER $name sh -c 'mkdir -p $CT_HOME/.config/paperboat && cat > $CT_HOME/.config/paperboat/config.json'"
# the member's CLI session is the controlled teammate profile (teammate_signin.py), apart from the device's own identity;
# /home/pbt/pbm runs pb as that profile (team_two_account.py --member-pb)
python3 "$here/teammate_signin.py" "$host ${CT_DOCKER%%docker}" "$name" $CT_USER >/dev/null
root=$CT_HOME/.local/state/paperboat/device-smoke/teammate
printf '#!/bin/sh\nexport HOME=%s/home XDG_CONFIG_HOME=%s/config XDG_DATA_HOME=%s/data XDG_STATE_HOME=%s/state XDG_CACHE_HOME=%s/cache XDG_RUNTIME_DIR=$CT_RUNDIR\nexec %s --config %s/config.json "$@"\n' \
  "$root" "$root" "$root" "$root" "$root" "$pbx" "$root" |
  ssh "$host" "$CT_DOCKER exec -i -u $CT_USER $name sh -c 'cat > $CT_HOME/pbm; chmod 755 $CT_HOME/pbm'"
ct "$CT_HOME/pbm machine add --json" | python3 -c '
import json, re, sys, urllib.request
command = json.load(sys.stdin)["data"]["linux_macos_command"]
sys.stdout.write(re.search(r"PB_TOKEN=\x27([A-Za-z0-9]+)\x27", command).group(1))
' | ssh "$host" "$CT_DOCKER exec -i -u $CT_USER -e HOME=$CT_HOME -e XDG_RUNTIME_DIR=$CT_RUNDIR $name sh -c 'cat > $CT_HOME/tok; chmod 600 $CT_HOME/tok'"
ct "$pbx pair --name $name --enrollment-token-file $CT_HOME/tok --json" | python3 -c 'import json,sys; d=json.load(sys.stdin); d=d.get("data",d); print(json.dumps({"name":"'"$name"'","machine_id":d["machine_id"]}))'
ct rm -f $CT_HOME/tok
