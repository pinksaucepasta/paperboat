#!/usr/bin/env bash
# Cut a throwaway machine container's network from the inside (it runs --privileged), for checks whose Windows client
# must keep its pb.exe running: a Windows Firewall rule only blocks new flows (winhost.NET_CUT stops pb.exe to make
# them new), so terminal_matrix cuts the peer instead (SMOKE_PEER_CUT). A capture in the container's network namespace
# proves the cut: `remove` prints the packets that left and arrived on eth0 while it held.
#
#   container_cut.sh SSH_HOST CONTAINER add all            every packet except loopback, both directions
#   container_cut.sh SSH_HOST CONTAINER add server IP...   outgoing packets to those addresses
#   container_cut.sh SSH_HOST CONTAINER add addresses IP... packets to and from those addresses
#   container_cut.sh SSH_HOST CONTAINER add silent IP...    the same, dropped on the host as they cross it (DOCKER-USER):
#                                                           the container's sends succeed, as on a real dead path (a drop
#                                                           in its own OUTPUT fails sendmsg at once and tells pb)
#   container_cut.sh SSH_HOST CONTAINER remove
set -euo pipefail
host=$1 ct=$2 action=$3; shift 3
case $action in
add)
  mode=$1; shift
  if [ "$mode" = silent ]; then
    ssh "$host" "set -e; ct_ip=\$(sudo docker exec $ct sh -c \"ip -4 -o addr show eth0 | awk '{print \\\$4}' | cut -d/ -f1\")
      for ip in $*; do sudo iptables -I DOCKER-USER -s \$ct_ip -d \$ip -m comment --comment pbcut-$ct -j DROP
                       sudo iptables -I DOCKER-USER -s \$ip -d \$ct_ip -m comment --comment pbcut-$ct -j DROP; done
      sudo sh -c 'nohup tcpdump -nni any -w /tmp/pbcut-$ct.pcap $(printf 'host %s' "$(echo "$@" | sed 's/ / or host /g')") >/dev/null 2>&1 & echo \$! >/tmp/pbcut-$ct.pid'
      sleep 1"
    exit
  fi
  tool() { case $1 in *:*) echo ip6tables ;; *) echo iptables ;; esac; }
  if [ "$mode" = all ]; then
    rules=$(for t in iptables ip6tables; do printf -- '%s -I OUTPUT ! -o lo -m comment --comment pbcut -j DROP;%s -I INPUT ! -i lo -m comment --comment pbcut -j DROP;' $t $t; done)
  elif [ "$mode" = server ]; then
    rules=$(for ip in "$@"; do printf -- '%s -I OUTPUT -d %s -m comment --comment pbcut -j DROP;' "$(tool "$ip")" "$ip"; done)
  else  # addresses: both directions
    rules=$(for ip in "$@"; do t=$(tool "$ip"); printf -- '%s -I OUTPUT -d %s -m comment --comment pbcut -j DROP;%s -I INPUT -s %s -m comment --comment pbcut -j DROP;' $t "$ip" $t "$ip"; done)
  fi
  filter=$([ "$mode" = all ] && echo "not port 22" || printf 'host %s' "$(echo "$@" | sed 's/ / or host /g')")
  # the capture starts once every rule is in place: a packet it records left despite the cut
  ssh "$host" "set -e; pid=\$(sudo docker inspect -f '{{.State.Pid}}' $ct)
    IFS=';'; for r in \$(echo '$rules'); do [ -n \"\$r\" ] && sudo docker exec $ct sh -c \"\$r\"; done
    sudo nsenter -t \$pid -n sh -c 'nohup tcpdump -nni eth0 -w /tmp/pbcut-$ct.pcap $filter >/dev/null 2>&1 & echo \$! >/tmp/pbcut-$ct.pid'
    sleep 1"
  ;;
remove)
  if ssh "$host" "sudo iptables -S DOCKER-USER | grep -q pbcut-$ct"; then
    ssh "$host" "sudo kill \$(cat /tmp/pbcut-$ct.pid) 2>/dev/null; sleep 1  # the capture ends while the rules still hold
      dropped=\$(sudo iptables -vnxL DOCKER-USER | awk '/pbcut-$ct/ {n+=\$1} END {print n+0}')
      while sudo iptables -S DOCKER-USER | grep -q pbcut-$ct; do sudo iptables -D DOCKER-USER \$(sudo iptables -S DOCKER-USER | grep pbcut-$ct | head -1 | cut -d' ' -f3-); done
      wire=\$(sudo tcpdump -nnr /tmp/pbcut-$ct.pcap 2>/dev/null | grep -v ' docker0 \| veth' | grep ' Out ' | grep -c ' UDP')
      inner=\$(sudo tcpdump -nnr /tmp/pbcut-$ct.pcap 2>/dev/null | grep -c ' docker0 *In ')
      sudo rm -f /tmp/pbcut-$ct.pcap /tmp/pbcut-$ct.pid
      echo \"capture during the cut on $host: sent=\$wire UDP packets to the wire, \$inner from $ct dropped silently (rule counters \$dropped)\""
    exit
  fi
  ssh "$host" "ip=\$(sudo docker exec $ct sh -c \"ip -4 -o addr show eth0 | awk '{print \\\$4}' | cut -d/ -f1\")
    own=\$(sudo docker exec $ct sh -c \"ip -o addr show eth0 | awk '{print \\\$4}' | cut -d/ -f1\" | sed 's/^/src host /' | paste -sd'|' | sed 's/|/ or /g; s/^/(ip or ip6) and (/; s/\$/)/')
    sudo kill \$(cat /tmp/pbcut-$ct.pid) 2>/dev/null; sleep 1  # the capture ends while every rule still holds
    for t in iptables ip6tables; do
      while sudo docker exec $ct \$t -S | grep -q pbcut; do
        r=\$(sudo docker exec $ct \$t -S | grep pbcut | head -1 | sed 's/^-A/-D/'); sudo docker exec $ct sh -c \"\$t \$r\"
      done
    done
    # IP only: ARP is not netfilter's (iptables cannot drop it) and carries no pb traffic
    sent=\$(sudo tcpdump -nnr /tmp/pbcut-$ct.pcap \"\$own\" 2>/dev/null); out=\$(printf '%s' \"\$sent\" | grep -c .)
    in=\$(sudo tcpdump -nnr /tmp/pbcut-$ct.pcap ip dst \$ip 2>/dev/null | wc -l)
    printf '%s\\n' \"\$sent\" | head -5 | grep . | sed 's/^/  sent: /' >&2
    arp=\$(sudo tcpdump -nnr /tmp/pbcut-$ct.pcap arp 2>/dev/null | wc -l)
    sudo rm -f /tmp/pbcut-$ct.pcap /tmp/pbcut-$ct.pid
    echo \"capture during the cut on $ct eth0: sent=\$out received=\$in (IP packets; ARP \$arp)\""
  ;;
*) echo "usage: container_cut.sh SSH_HOST CONTAINER add all|server IP...|remove" >&2; exit 2 ;;
esac
