# fresh_pair_lib.sh: sourced on a Docker host (laneh-ubuntu24-systemd image, whose user `pbt` has uid 1000 and
# passwordless sudo). The host's own `pb` is signed in to the test account and only mints enrollment tokens; set
# FRESH_MINT to another signed-in pb command to mint for a second account. The enrollment token stays in files.

start_ct() { # start_ct CONTAINER: a fresh systemd container
  sudo -n docker run -d --name $1 --privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock laneh-ubuntu24-systemd >/dev/null
  sleep 8
  # The image has no SSH server; managed SSH (pb ssh) needs one with keys of its own on every machine.
  sudo -n docker exec -e DEBIAN_FRONTEND=noninteractive $1 bash -c 'apt-get -qq update && apt-get -qq install -y openssh-server && rm -f /etc/ssh/ssh_host_* && ssh-keygen -A && systemctl enable --now ssh' >/dev/null 2>&1
}

add_user() { # add_user CONTAINER USER UID: a second OS user like pbt (passwordless sudo, lingering)
  sudo -n docker exec $1 bash -c "useradd -m -u $3 -s /bin/bash $2 && echo '$2 ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/$2 && chmod 440 /etc/sudoers.d/$2"
}

pair_user() { # pair_user CONTAINER USER UID NAME: install $FRESH_BINARY as USER and pair as NAME; prints MACHINE_ID=
  local ct=$1 user=$2 uid=$3 name=$4 mint=${FRESH_MINT:-$HOME/.local/bin/pb}
  local X="sudo -n docker exec -u $user -e HOME=/home/$user -e XDG_RUNTIME_DIR=/run/user/$uid $ct"
  sudo -n docker exec $ct loginctl enable-linger $user
  sudo -n docker exec $ct systemctl start user@$uid.service
  sudo -n docker cp "$FRESH_BINARY" $ct:/home/$user/pb
  sudo -n docker exec $ct bash -c "chown $user:$user /home/$user/pb; chmod 755 /home/$user/pb"
  $X bash -c 'cd ~ && ./pb install >/dev/null'
  $mint machine add --name $name --json | TOKEN=/tmp/$ct-$user-token python3 -c '
import json, os, re, subprocess, sys
added = json.load(sys.stdin)
url = re.search(r"'"'"'(https?://[^'"'"']+)'"'"'", added["data"]["linux_macos_command"]).group(1)
script = subprocess.run(["curl", "-fsSL", url], capture_output=True, text=True, check=True).stdout
token = re.search(r"PAPERBOAT_ENROLLMENT_TOKEN='"'"'([A-Za-z0-9]+)'"'"'", script).group(1)
with open(os.open(os.environ["TOKEN"], os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600), "w") as f:
    f.write(token)
'
  sudo -n docker cp /tmp/$ct-$user-token $ct:/home/$user/token
  rm -f /tmp/$ct-$user-token
  sudo -n docker exec $ct bash -c "chown $user:$user /home/$user/token; chmod 600 /home/$user/token"
  $X /home/$user/.local/bin/pb pair --enrollment-token-file /home/$user/token --name $name --json | python3 -c 'import json,sys;d=json.load(sys.stdin);assert d["ok"],d;print("MACHINE_ID="+d["data"]["machine_id"])'
  $X rm -f /home/$user/token
}

fresh() { # fresh CONTAINER NAME: a fresh container with pbt paired as NAME
  start_ct $1
  pair_user $1 pbt 1000 $2
}

fresh_official() { # fresh_official CONTAINER NAME: a fresh container, the published one-liner (the official Latest), freshly enrolled; prints MACHINE_ID= and OFFICIAL_<container>=<version>
  local ct=$1 name=$2 X="sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 $1" mint=${FRESH_MINT:-$HOME/.local/bin/pb}
  start_ct $ct
  sudo -n docker exec $ct bash -c "loginctl enable-linger pbt; systemctl start user@1000.service"
  $mint machine add --name $name --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["data"]["linux_macos_command"])' > /tmp/$ct-cmd
  sudo -n docker cp /tmp/$ct-cmd $ct:/home/pbt/install-cmd; rm -f /tmp/$ct-cmd
  $X bash -c 'bash /home/pbt/install-cmd > /home/pbt/install.log 2>&1; rm -f /home/pbt/install-cmd'
  $X python3 -c 'import json;print("MACHINE_ID="+json.load(open("/home/pbt/.local/state/paperboat/runtime/machine-registration.json"))["machine_id"])'
  echo "OFFICIAL_$ct=$($X /home/pbt/.local/bin/pb --version | sed -n 's/.*Version //p' | head -1)"
}

fresh_release() { # fresh_release CONTAINER NAME VERSION: the same with an OLDER official release. The published one-liner only installs the
  # newest, so the older release's asset comes from the place the installer downloads it, and is installed and paired like any supplied binary.
  local ct=$1 name=$2 rel=$3 mint=${FRESH_MINT:-$HOME/.local/bin/pb} url
  start_ct $ct
  sudo -n docker exec $ct bash -c "loginctl enable-linger pbt; systemctl start user@1000.service"
  url=$($mint machine add --name $name --json | python3 -c '
import json, re, subprocess, sys
cmd = json.load(sys.stdin)["data"]["linux_macos_command"]
script = subprocess.run(["curl", "-fsSL", re.search(r"'"'"'(https?://[^'"'"']+)'"'"'", cmd).group(1)], capture_output=True, text=True, check=True).stdout
print(re.search(r"product_url='"'"'([^'"'"']*linux-amd64)'"'"'", script).group(1))')
  curl -fsSL -o /tmp/$ct-rel "$(sed -E "s#/download/[^/]+/#/download/$rel/#" <<<"$url")"
  FRESH_BINARY=/tmp/$ct-rel pair_user $ct pbt 1000 $name
  rm -f /tmp/$ct-rel
  echo "OFFICIAL_$ct=$(sudo -n docker exec -u pbt -e HOME=/home/pbt $ct /home/pbt/.local/bin/pb --version | sed -n 's/.*Version //p' | head -1)"
}
