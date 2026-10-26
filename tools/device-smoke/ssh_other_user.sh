#!/bin/bash
# ssh_other_user.sh HOST BINARY  (check id: ssh-other-user, CHECKLIST F5-11)
#
# Managed SSH applies only to the OS user who set pb up on the machine. Any other OS user reaches the machine
# through the same Paperboat transport and authenticates with their own key, agent or password; we never refuse
# them and never offer them the managed key. Pairs two FRESH throwaway containers on HOST (a Linux VM with
# Docker and the laneh-ubuntu24-systemd image whose user `pb` is signed in to the test account), the target with
# OpenSSH and a second OS user `bob` (own key + password), then from the client container checks, for BOTH
# entry points (`pb ssh` and plain `ssh bob@<alias>.<domain>`):
#   setup user : managed key logs in (baseline)
#   bob        : own key via plain ssh; own key via the agent through `pb ssh`; password through plain ssh and
#                through `pb ssh`; a wrong password fails with sshd's own "Permission denied" text
# Prints one JSON line {"check":"ssh-other-user","status":"PASS"|"FAIL",...}. Machines are revoked by exact id
# and containers removed on every exit path; credentials stay in files, never printed.
set -uo pipefail
HOST=${1:?host} BIN=${2:?binary}
HERE=$(cd "$(dirname "$0")" && pwd)
STAMP=$(date +%s)
T=lanei-sou-t-$STAMP C=lanei-sou-c-$STAMP ALIAS=lanei-sou-$STAMP
IDS=
finish() {
  local status=$1 detail=$2 id
  ssh "$HOST" "sudo -n docker rm -f $T $C >/dev/null 2>&1; rm -f /tmp/$T-pb" || true
  for id in $IDS; do python3 "$HERE/cleanup_test_account.py" --id "$id" --stale '0 seconds' --apply >/dev/null 2>&1 || detail="$detail; revoke of $id failed"; done
  python3 -c 'import json,sys;print(json.dumps({"check":"ssh-other-user","status":sys.argv[1],"machine_ids":sys.argv[2].split(),"detail":sys.argv[3]}))' "$status" "$IDS" "$detail"
  [ "$status" = PASS ]
  exit $?
}
scp -q "$BIN" "$HOST:/tmp/$T-pb" || finish FAIL "could not copy the build to $HOST"
out=$(ssh "$HOST" "bash -s" <<EOF 2>&1
set -euo pipefail
fresh() { # fresh CONTAINER NAME PACKAGES
  local ct=\$1 name=\$2 packages=\$3
  local X="sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 \$ct"
  sudo -n docker run -d --name \$ct --privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock laneh-ubuntu24-systemd >/dev/null
  sleep 8
  sudo -n docker exec \$ct bash -c "DEBIAN_FRONTEND=noninteractive apt-get -qq update >/dev/null 2>&1; DEBIAN_FRONTEND=noninteractive apt-get -qq install -y \$packages >/dev/null 2>&1; true"
  sudo -n docker exec \$ct loginctl enable-linger pbt
  sudo -n docker exec \$ct systemctl start user@1000.service
  sudo -n docker cp /tmp/$T-pb \$ct:/home/pbt/pb
  sudo -n docker exec \$ct bash -c 'chown pbt:pbt /home/pbt/pb; chmod 755 /home/pbt/pb'
  \$X bash -c 'cd ~ && ./pb install >/dev/null'
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
}
# the target: OpenSSH (paired AFTER sshd runs, so the setup user pbt is registered with port 22) + user bob
fresh $T $ALIAS "openssh-server"
sudo -n docker exec $T bash -c 'systemctl enable --now ssh >/dev/null 2>&1; useradd -m -s /bin/bash bob; echo "bob:\$(cat /proc/sys/kernel/random/uuid)" | chpasswd; printf "PasswordAuthentication yes\nKbdInteractiveAuthentication no\n" > /etc/ssh/sshd_config.d/99-bob.conf; systemctl reload ssh'
fresh $C $ALIAS-c "openssh-client sshpass"
EOF
)
IDS=$(sed -n 's/^MACHINE_ID=\(machine_[0-9a-f-]*\)$/\1/p' <<<"$out" | tr '\n' ' ')
[ -n "$IDS" ] || finish FAIL "pairing failed: $(tail -c 300 <<<"$out" | tr '\n' ' ')"
# bob's credentials: a key pair made on the client, a password set on the target; neither is printed
cat > /tmp/$T-run.sh <<'RUN'
set -u
ALIAS=$1; T=$2
PB=~/.local/bin/pb
fail=""; ok=""
check() { # check NAME EXPECT_SUBSTRING COMMAND...
  local name=$1 want=$2; shift 2
  local out
  out=$("$@" 2>&1 </dev/null)
  if grep -q -- "$want" <<<"$out"; then ok="$ok $name"; else fail="$fail $name($(tr '\n' ' ' <<<"$out" | cut -c1-160))"; fi
}
for i in $(seq 1 60); do "$PB" machine list 2>/dev/null | grep -q "$ALIAS " && break; sleep 2; done
DOMAIN=$(grep -o 'Host \*\.[^ ]*' ~/.ssh/paperboat_config 2>/dev/null | head -1 | sed 's/Host \*\.//')
for i in $(seq 1 60); do [ -n "$DOMAIN" ] && break; sleep 2; DOMAIN=$(grep -o 'Host \*\.[^ ]*' ~/.ssh/paperboat_config 2>/dev/null | head -1 | sed 's/Host \*\.//'); done
HOST=$ALIAS.$DOMAIN
echo "domain=$DOMAIN"
check setup-user-pb "ok-managed" "$PB" ssh "$ALIAS" -- echo ok-managed
check setup-user-plain "ok-managed" ssh "pbt@$HOST" echo ok-managed
# bob's own key (made by the caller on this client, trusted by the target)
check bob-key-plain "ok-bob-key" ssh -i ~/bob_key -o IdentitiesOnly=yes -o BatchMode=yes "bob@$HOST" echo ok-bob-key
eval "$(ssh-agent -s)" >/dev/null
ssh-add -q ~/bob_key
check bob-agent-pb "ok-bob-agent" "$PB" ssh "bob@$ALIAS" -- echo ok-bob-agent
check bob-agent-plain "ok-bob-agent" ssh -o BatchMode=yes "bob@$HOST" echo ok-bob-agent
ssh-agent -k >/dev/null 2>&1; unset SSH_AUTH_SOCK
# password: through sshpass' pty; a wrong password must fail with sshd's own text, not ours
export SSHPASS
SSHPASS=$(cat ~/bob_pw)
check bob-password-plain "ok-bob-password" sshpass -e ssh -o PubkeyAuthentication=no -o PreferredAuthentications=password,keyboard-interactive "bob@$HOST" echo ok-bob-password
check bob-password-pb "ok-bob-password" sshpass -e "$PB" ssh "bob@$ALIAS" -- echo ok-bob-password
SSHPASS=wrong-password-1
check bob-wrong-password-is-sshd "Permission denied" sshpass -e ssh -o PubkeyAuthentication=no -o NumberOfPasswordPrompts=1 "bob@$HOST" echo no
echo "RESULT ok:[$ok ] fail:[$fail ]"
RUN
ssh "$HOST" "sudo -n docker exec -i -u pbt $C sh -c 'cat > /home/pbt/run.sh'" < /tmp/$T-run.sh; rm -f /tmp/$T-run.sh
# install bob's public key and a known password on the target, put the password where only the client's pbt reads it
res=$(ssh "$HOST" "bash -s" <<EOF 2>&1
set -euo pipefail
PW=\$(head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 20)
sudo -n docker exec $T bash -c "echo bob:\$PW | chpasswd"
printf %s "\$PW" | sudo -n docker exec -i -u pbt $C bash -c 'umask 077; cat > ~/bob_pw'
sudo -n docker exec -u pbt $C bash -c 'umask 077; ssh-keygen -q -t ed25519 -N "" -f /home/pbt/bob_key; cat /home/pbt/bob_key.pub' | sudo -n docker exec -i $T bash -c 'install -d -m 700 -o bob -g bob /home/bob/.ssh; cat > /home/bob/.ssh/authorized_keys; chown bob:bob /home/bob/.ssh/authorized_keys; chmod 600 /home/bob/.ssh/authorized_keys'
sudo -n docker exec -u pbt $C rm -f /home/pbt/bob_key.pub
sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 $C timeout 600 bash /home/pbt/run.sh $ALIAS $T
EOF
)
detail=$(grep -E "^(domain=|RESULT)" <<<"$res" | tr '\n' ' ')
if grep -q "RESULT ok:\[ setup-user-pb setup-user-plain bob-key-plain bob-agent-pb bob-agent-plain bob-password-plain bob-password-pb bob-wrong-password-is-sshd \] fail:\[ \]" <<<"$res"; then
  finish PASS "$detail"
fi
finish FAIL "$detail $(tail -c 300 <<<"$res" | tr '\n' ' ')"
