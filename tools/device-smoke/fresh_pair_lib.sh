# fresh_pair_lib.sh: sourced on a Docker host (laneh-ubuntu24-systemd image; its user `pb` is signed in to the test
# account and only mints enrollment tokens). fresh CONTAINER NAME starts a throwaway container, installs
# $FRESH_BINARY as user pbt, pairs it as NAME and prints MACHINE_ID=<id>. The enrollment token stays in files.
fresh() { # fresh CONTAINER NAME: start, install the build, pair as NAME; prints MACHINE_ID=
  local ct=$1 name=$2
  local X="sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 $ct"
  sudo -n docker run -d --name $ct --privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock laneh-ubuntu24-systemd >/dev/null
  sleep 8
  sudo -n docker exec $ct loginctl enable-linger pbt
  sudo -n docker exec $ct systemctl start user@1000.service
  sudo -n docker cp "$FRESH_BINARY" $ct:/home/pbt/pb
  sudo -n docker exec $ct bash -c 'chown pbt:pbt /home/pbt/pb; chmod 755 /home/pbt/pb'
  $X bash -c 'cd ~ && ./pb install >/dev/null'
  NAME=$name TOKEN=/tmp/$ct-token python3 - <<'PY'
import json, os, re, subprocess
added = json.loads(subprocess.run([os.path.expanduser("~/.local/bin/pb"), "machine", "add", "--name", os.environ["NAME"], "--json"], capture_output=True, text=True, check=True).stdout)
url = re.search(r"'(https?://[^']+)'", added["data"]["linux_macos_command"]).group(1)
script = subprocess.run(["curl", "-fsSL", url], capture_output=True, text=True, check=True).stdout
token = re.search(r"PAPERBOAT_ENROLLMENT_TOKEN='([A-Za-z0-9]+)'", script).group(1)
with open(os.open(os.environ["TOKEN"], os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600), "w") as f:
    f.write(token)
PY
  sudo -n docker cp /tmp/$ct-token $ct:/home/pbt/token
  rm -f /tmp/$ct-token
  sudo -n docker exec $ct bash -c 'chown pbt:pbt /home/pbt/token; chmod 600 /home/pbt/token'
  $X /home/pbt/.local/bin/pb pair --enrollment-token-file /home/pbt/token --name $name --json | python3 -c 'import json,sys;d=json.load(sys.stdin);assert d["ok"],d;print("MACHINE_ID="+d["data"]["machine_id"])'
  $X rm -f /home/pbt/token
}
