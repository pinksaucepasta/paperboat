#!/bin/sh
# One-time setup of a canary machine (R-06): a systemd container on HOST ('local' or an ssh target) paired as
# machine NAME with the given pb build. With 'origin', the machine also gets the canary origin (a user service
# serving ~/canary-www on CANARY_PORT) and the permanent canary tunnel to it; the tunnel's URL is printed last.
#   TOKEN_CMD  prints one fresh enrollment token for the name given as its last argument, on stdout only
#              (it is piped into the container, never shown), e.g. 'ssh pbvm2 python3 laneh/gettoken.py'
# usage: TOKEN_CMD='...' canary-setup.sh <host> <container> <machine-name> <pb-binary> [origin]
# Updating pb later: copy the new binary to /home/pbt/pb in the container and run `./pb install` as pbt.
set -eu
host=$1 ct=$2 name=$3 bin=$4 origin=${5:-} port=${CANARY_PORT:-18099}
: "${TOKEN_CMD:?}"
here=$(cd "$(dirname "$0")" && pwd)
on() { if [ "$host" = local ]; then sh -c "$1"; else ssh -o BatchMode=yes "$host" "$1"; fi; }
as_pbt() { on "sudo -n docker exec -i -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 -w /home/pbt $ct $1"; }
on "sudo -n docker build -q -t pb-canary -" <"$here/canary.Dockerfile"
on "sudo -n docker rm -f $ct >/dev/null 2>&1; sudo -n docker run -d --name $ct --hostname $name --restart unless-stopped \
  --memory 768m --privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock pb-canary"
for _ in $(seq 30); do on "sudo -n docker exec $ct systemctl is-system-running --wait" >/dev/null 2>&1 && break; sleep 2; done
# every container of the image starts with the same baked SSH host keys; Paperboat refuses a clone's keys
on "sudo -n docker exec $ct sh -c 'rm -f /etc/ssh/ssh_host_*; ssh-keygen -A >/dev/null; systemctl restart ssh'"
on "sudo -n docker exec $ct sh -c 'loginctl enable-linger pbt && systemctl start user@1000.service'"
on "sudo -n docker exec -i $ct sh -c 'cat >/home/pbt/pb && chmod 755 /home/pbt/pb && chown pbt:pbt /home/pbt/pb'" <"$bin"
as_pbt "./pb install"
sh -c "$TOKEN_CMD $name" | on "sudo -n docker exec -i $ct sh -c 'umask 077; cat >/home/pbt/token; chown pbt /home/pbt/token'"
as_pbt ".local/bin/pb pair --enrollment-token-file /home/pbt/token --name $name --json" | head -c 400; echo
as_pbt "rm -f /home/pbt/token"
[ "$origin" = origin ] || exit 0
as_pbt "sh -c 'mkdir -p canary-www .config/systemd/user && echo ok >canary-www/index.html && printf \"%s\n\" \
  \"[Service]\" \"ExecStart=/usr/bin/python3 -m http.server $port --bind 127.0.0.1 --directory /home/pbt/canary-www\" \
  \"Restart=always\" \"[Install]\" \"WantedBy=default.target\" >.config/systemd/user/canary-origin.service && \
  systemctl --user daemon-reload && systemctl --user enable --now canary-origin.service'"
as_pbt ".local/bin/pb tunnel create canary --port $port --wait --json" |
  python3 -c 'import json,sys; d=json.load(sys.stdin); d=d.get("data",d); print("CANARY_TUNNEL_URL=" + d["tunnel"]["stable_endpoint"])'
