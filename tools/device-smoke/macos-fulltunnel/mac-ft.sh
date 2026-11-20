#!/bin/bash
# Runs ON the Mac (detached, so the cut of ssh does not matter): brings the WireGuard full tunnel up, checks pb from the Mac side, takes it down.
# Markers: /tmp/x38r-ft.up (tunnel up and Mac-initiated checks done: the driver runs L>M and W>M now), /tmp/x38r-ft.done. Log: /tmp/x38r-ft.log
# Needs /tmp/x38rw.conf (AllowedIPs 0.0.0.0/0). A watchdog takes the tunnel down after 10 min whatever happens.
export PATH=/opt/homebrew/bin:$PATH; PB=/Users/admin/.local/bin/pb; CONF=/tmp/x38rw.conf; exec >/tmp/x38r-ft.log 2>&1
rm -f /tmp/x38r-ft.up /tmp/x38r-ft.done
( sleep 600; sudo -n wg-quick down $CONF >/dev/null 2>&1 ) & WD=$!
echo "== before"; route -n get default | grep -E "gateway|interface"; $PB version | grep Version
sudo -n wg-quick up $CONF 2>&1 | sed 's/private-key.*//'
echo "== routes under the VPN"; netstat -rn -f inet | grep -E "^(0/1|128.0/1|default|[0-9.]+ +[0-9.]+ +UGHS)" | head -8
echo "== egress"; curl -s -m 10 -o /dev/null -w "code=%{http_code} remote=%{remote_ip}\n" "${PB_API_URL:?set PB_API_URL to the API address}"
sleep 40
echo "== doctor under the VPN (Mac)"; $PB doctor 2>&1 | head -22 | cut -c1-200
for p in rx38r-l test-w test-l2; do
  echo "== ping $p (Mac initiator)"; $PB ping $p --count 3 2>&1 | tail -3
  echo "== exec $p (Mac initiator)"; $PB exec $p -- $([ $p = test-w ] && echo cmd.exe /c echo ok-from-mac || echo echo ok-from-mac) 2>&1 | tail -2
done
touch /tmp/x38r-ft.up; sleep 100
echo "== doctor under the VPN (Mac), second look"; $PB doctor 2>&1 | head -3
sudo -n wg-quick down $CONF 2>&1 | tail -2; kill $WD 2>/dev/null
echo "== after down"; route -n get default | grep -E "gateway|interface"; sleep 15; $PB doctor 2>&1 | head -2
touch /tmp/x38r-ft.done
