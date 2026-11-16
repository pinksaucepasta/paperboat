#!/bin/bash
# Runs ON a Mac with ONE network interface (a GitHub macpool runner): the network service is switched DHCP -> a manual address of
# another network configuration (new source address, same link) and back, under a Mac-initiated exec to $1 (a paired machine).
# macOS "network switch" as an application sees it: its local address changes while sessions run. Detached; log /tmp/x38r-as.log, marker /tmp/x38r-as.done.
# Needs passwordless sudo (macpool runner). A watchdog restores DHCP after 7 min whatever happens.
PB=${PB:-$HOME/.local/bin/pb}; PEER=${1:?peer}; N=150; exec >/tmp/x38r-as.log 2>&1
rm -f /tmp/x38r-as.done
# a macpool runner has one network service ("Ethernet"): the first one listed after the header line
SVC=$(networksetup -listallnetworkservices | sed -n '2p' | sed 's/^\*//'); DEV=$(route -n get default | awk '/interface:/{print $2}')
IP=$(ipconfig getifaddr $DEV); MASK=$(ipconfig getoption $DEV subnet_mask); GW=$(route -n get default | awk '/gateway:/{print $2}')
echo "service='$SVC' dev=$DEV ip=$IP mask=$MASK gw=$GW"
restore() { sudo -n networksetup -setdhcp "$SVC"; }
( sleep 420; restore ) & WD=$!
# a free neighbour address: the last octet moved until nothing answers
NEWIP=""; for o in 201 202 203 204 205; do c="${IP%.*}.$o"; ping -c 1 -t 1 $c >/dev/null 2>&1 || { NEWIP=$c; break; }; done
echo "new address: ${NEWIP:-none free}"; [ -n "$NEWIP" ] || { kill $WD; touch /tmp/x38r-as.done; exit 0; }
src() { curl -s -m 8 -o /dev/null -w "egress src=%{local_ip} http=%{http_code}" https://api.pprbt.dev/; }
echo "== before: $(src)"; touch /tmp/x38r-as.up
( $PB exec $PEER -- sh -c 'for i in $(seq 1 '$N'); do echo $i; sleep 1; done' > /tmp/x38r-as.exec 2>/tmp/x38r-as.exec.err; echo "exec rc=$?" > /tmp/x38r-as.exec.rc ) & EX=$!
sleep 30
echo "== t+30 manual address $NEWIP"; sudo -n networksetup -setmanual "$SVC" $NEWIP $MASK $GW; sleep 10; echo "after switch: $(src)  ifaddr=$(ipconfig getifaddr $DEV)"
echo "ping: $($PB ping $PEER --count 2 2>&1 | tail -1)"
sleep 45
echo "== t+90 DHCP again"; sudo -n networksetup -setdhcp "$SVC"; sleep 12; echo "after switch back: $(src)  ifaddr=$(ipconfig getifaddr $DEV)"
wait $EX; echo "Mac-initiated exec: $(cat /tmp/x38r-as.exec.rc) lines=$(grep -c '^[0-9]' /tmp/x38r-as.exec) of $N"
sleep 20; echo "ping after: $($PB ping $PEER --count 3 2>&1 | tail -2 | tr '\n' ' ')"
kill $WD 2>/dev/null; echo "== restored: $(src) ifaddr=$(ipconfig getifaddr $DEV)"
touch /tmp/x38r-as.done
