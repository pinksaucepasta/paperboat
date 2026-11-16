#!/bin/bash
# WireGuard full-tunnel endpoint with IPv4 NAT on a Linux bed host (X-38 macOS full-tunnel). Own interface, own subnet, rules tagged x38r.
# usage: bed-wg.sh up | peer <pubkey> | down        env: BED (ssh host, default pbvm2), WGIF (x38rw), PORT (51899), NET (10.38.7)
# up prints the bed's public key.
set -eu
BED=${BED:-pbvm2}; WGIF=${WGIF:-x38rw}; PORT=${PORT:-51899}; NET=${NET:-10.38.7}
case ${1:-} in
up) ssh "$BED" "set -e; sudo ip link del $WGIF 2>/dev/null || true; sudo ip link add $WGIF type wireguard; umask 077; wg genkey > /tmp/$WGIF.key
  sudo wg set $WGIF private-key /tmp/$WGIF.key listen-port $PORT; sudo ip addr add $NET.1/24 dev $WGIF; sudo ip link set $WGIF up
  sudo sysctl -qw net.ipv4.ip_forward=1; OUT=\$(ip -4 route show default | sed -n 's/.* dev \\([^ ]*\\).*/\\1/p' | head -1)
  sudo iptables -I FORWARD 1 -i $WGIF -m comment --comment x38r -j ACCEPT; sudo iptables -I FORWARD 1 -o $WGIF -m comment --comment x38r -j ACCEPT
  sudo iptables -t nat -A POSTROUTING -s $NET.0/24 -o \$OUT -m comment --comment x38r -j MASQUERADE
  for d in -i -o; do sudo iptables -t mangle -A FORWARD \$d $WGIF -p tcp --tcp-flags SYN,RST SYN -m comment --comment x38r -j TCPMSS --clamp-mss-to-pmtu; done
  sudo wg show $WGIF public-key" ;;
peer) ssh "$BED" "sudo wg set $WGIF peer '$2' allowed-ips $NET.2/32" ;;
down) ssh "$BED" "while sudo iptables -S FORWARD | grep -q x38r; do sudo iptables \$(sudo iptables -S FORWARD | grep x38r | head -1 | sed 's/^-A/-D/'); done
  while sudo iptables -t nat -S POSTROUTING | grep -q x38r; do sudo iptables -t nat \$(sudo iptables -t nat -S POSTROUTING | grep x38r | head -1 | sed 's/^-A/-D/'); done
  while sudo iptables -t mangle -S FORWARD | grep -q x38r; do sudo iptables -t mangle \$(sudo iptables -t mangle -S FORWARD | grep x38r | head -1 | sed 's/^-A/-D/'); done
  sudo ip link del $WGIF 2>/dev/null || true; rm -f /tmp/$WGIF.key" ;;
*) echo "usage: $0 up|peer <pubkey>|down" >&2; exit 2 ;;
esac
