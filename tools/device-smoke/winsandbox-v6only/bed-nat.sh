#!/bin/bash
# IPv4 NAT for the sandbox full-tunnel phase on the bed host, with a pinned UDP source-port range so a "NAT rebinding"
# can be done by changing the range and flushing the conntrack entries of the guest.
# usage: bed-nat.sh on|rebind [first port]|off|conntrack     env: BED (ssh host), WGIF, NET4 (tunnel subnet to NAT), GUEST4 (address whose conntrack entries rebind flushes)
set -eu
BED=${BED:-pbvm2}; WGIF=${WGIF:-x38ow}; G=${GUEST4:-10.38.0.2}
OUT=$(ssh "$BED" "ip -4 route show default | sed -n 's/.* dev \\([^ ]*\\).*/\\1/p' | head -1")
rules() { # $1 = first port of the 1000-port range
  ssh "$BED" "sudo -n iptables -t nat -A POSTROUTING -s ${NET4:-10.38.0.0/24} -o $OUT -p udp -m comment --comment x38q -j MASQUERADE --to-ports $1-$(($1 + 999))
  sudo -n iptables -t nat -A POSTROUTING -s ${NET4:-10.38.0.0/24} -o $OUT -m comment --comment x38q -j MASQUERADE"; }
del() { ssh "$BED" "while sudo -n iptables -t nat -D POSTROUTING \$(sudo -n iptables -t nat -S POSTROUTING | grep -- '--comment x38q' | head -1 | sed 's/^-A POSTROUTING //') 2>/dev/null; do :; done; true"; }
case ${1:-} in
on) ssh "$BED" "sudo -n sysctl -qw net.ipv4.ip_forward=1; sudo -n iptables -C FORWARD -i $WGIF -m comment --comment x38o-w -j ACCEPT"; del; rules 41000 ;;
rebind) del; rules ${2:-42000}; ssh "$BED" "sudo -n conntrack -D -s $G -p udp 2>&1 | tail -1" ;;
off) del ;;
conntrack) ssh "$BED" "sudo -n conntrack -L -s $G -p udp 2>/dev/null | head -20" ;;
*) echo "usage: $0 on|rebind [first port]|off|conntrack" >&2; exit 2 ;;
esac
