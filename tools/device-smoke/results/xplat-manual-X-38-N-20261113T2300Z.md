# X-38 by hand, lane R-X-38-N, UTC 2026-11-13 22:24 - 23:25 (IST 2026-11-14 03:54 - 04:55)

Build on every machine: `pb version` dev-ef42e84a9 (newest build of main; origin/main d47f51328 differs from it only in CHECKLIST.md and a results file).
Machines (all paired into the dev account, own throwaway names, own pools):
- rx38n-a: Linux (Ubuntu 24.04 container on pbvm2, dual-stack, default docker bridge behind the host NAT; moved to bridge x38n during the network-change check).
- rx38n-v6: Linux container on pbvm2, IPv6 ONLY: docker bridge x38nbr (fd3a:38::/64), NAT64 (tayga, 64:ff9b::/96) + DNS64 (unbound) on pbvm2, every IPv4 address and route removed (`ip -4 addr` shows 127.0.0.1 only), resolver = the DNS64 server. The prod api and edges are reached only through NAT64.
- rx38n-m: macOS 26.7.2 arm64, macpool-3 (GitHub runner, adopted), Application Firewall default.
- rx38n-w: Windows 11 (10.0.26100), winpool-4 (GitHub runner), Defender Firewall on (all three profiles) with the "Paperboat pb.exe" allow rules pb installs.

## Names: prod advertises DNS names for edges and relays (read-only prod DB)
`control_tunnel_nodes` (state ready): edge-oracle.pprbt.dev and edge-anvit.pprbt.dev, both as `endpoint_host` (the relay endpoint) and, for the current node version, as `carrier_endpoint_host`; no ready node advertises an IP literal (56 offline rows of an old version still hold 140.245.11.41). Nothing is missing for the DEP2 lane.

## IPv6 only on prod: PASS (L, with L, M and W peers)
== v6>a ping
sample 1: path=regional_relay connect=430.83ms
sample 2: path=regional_relay connect=121.40ms
sample 3: path=regional_relay connect=63.75ms
sample 4: path=regional_relay connect=64.89ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=63.75/170.22/430.83ms
== v6>a exec
609f8b344982
== a>v6 ping
sample 1: path=regional_relay connect=432.33ms
sample 2: path=regional_relay connect=119.02ms
sample 3: path=regional_relay connect=63.34ms
sample 4: path=regional_relay connect=72.25ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=63.34/171.74/432.33ms
== a>v6 exec
343406103e99
== v6 doctor a
peer_reachability: warning: Established an authenticated native connection through a relay.
selected_path: warning: The active Paperboat path uses a regional relay.

pb connect rx38n-v6 > rx38n-a (pty): shell on rx38n-a printed CONNECT-OK-609f8b344982 (its hostname).

## All pairs, ping x4 + exec hostname (path stated by pb ping)
```
### m > a  22:37:53Z
sample 1: path=regional_relay connect=828.61ms
sample 2: path=regional_relay connect=281.49ms
sample 3: path=regional_relay connect=282.20ms
sample 4: path=regional_relay connect=284.55ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=281.49/419.21/828.61ms
exec: 609f8b344982
### a > m  22:38:05Z
sample 1: path=regional_relay connect=846.33ms
sample 2: path=regional_relay connect=280.31ms
sample 3: path=regional_relay connect=282.87ms
sample 4: path=regional_relay connect=284.43ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=280.31/423.49/846.33ms
exec: sat12-bq155-796eb898-74a7-4e61-aa5b-1b691cd4876e-5A2EC5B81174.local
### w > a  22:38:12Z
sample 1: path=direct connect=2124.05ms
sample 2: path=direct connect=231.97ms
sample 3: path=direct connect=233.65ms
sample 4: path=direct connect=234.11ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=231.97/705.94/2124.05ms
exec: 
### a > w  22:38:25Z
sample 1: path=regional_relay connect=1782.77ms
sample 2: path=direct connect=250.21ms transition
sample 3: path=direct connect=226.75ms
sample 4: path=direct connect=230.90ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=226.75/622.66/1782.77ms
exec: runnervmm7ud1
### m > w  22:38:33Z
sample 1: path=direct connect=334.84ms
sample 2: path=direct connect=60.64ms
sample 3: path=direct connect=60.24ms
sample 4: path=direct connect=37.47ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=37.47/123.30/334.84ms
exec: runnervmm7ud1
### w > m  22:38:42Z
sample 1: path=regional_relay connect=4299.06ms
sample 2: path=regional_relay connect=492.30ms
sample 3: path=regional_relay connect=493.74ms
sample 4: path=regional_relay connect=497.05ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=492.30/1445.54/4299.06ms
exec: 
### m > v6  22:38:59Z
sample 1: path=regional_relay connect=2106.39ms
sample 2: path=regional_relay connect=281.41ms
sample 3: path=regional_relay connect=284.91ms
sample 4: path=regional_relay connect=282.69ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=281.41/738.85/2106.39ms
exec: 343406103e99
### v6 > m  22:39:14Z
sample 1: path=regional_relay connect=844.35ms
sample 2: path=regional_relay connect=284.19ms
sample 3: path=regional_relay connect=276.64ms
sample 4: path=regional_relay connect=286.29ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=276.64/422.87/844.35ms
exec: sat12-bq155-796eb898-74a7-4e61-aa5b-1b691cd4876e-5A2EC5B81174.local
### w > v6  22:39:22Z
sample 1: path=regional_relay connect=2081.34ms
sample 2: path=regional_relay connect=275.20ms
sample 3: path=regional_relay connect=263.80ms
sample 4: path=regional_relay connect=269.47ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=263.80/722.45/2081.34ms
exec: 
### v6 > w  22:39:35Z
sample 1: path=regional_relay connect=835.65ms
sample 2: path=regional_relay connect=265.73ms
sample 3: path=regional_relay connect=267.71ms
sample 4: path=regional_relay connect=266.87ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=265.73/408.99/835.65ms
exec: runnervmm7ud1
```

## UDP blocked on L (nft drop of every UDP packet except port 53, both directions): PASS L<->W
```
## UDP block on L (rx38n-a) 22:42:07Z
2
## blocked 22:42:09Z
### a w
sample 1: path=regional_relay connect=5555.72ms
sample 2: path=regional_relay connect=284.93ms
sample 3: path=regional_relay connect=255.15ms
sample 4: path=regional_relay connect=254.13ms
sample 5: path=regional_relay connect=256.55ms
sample 6: path=regional_relay connect=256.25ms
sample 7: path=regional_relay connect=255.42ms
sample 8: path=regional_relay connect=256.18ms
sample 9: path=regional_relay connect=256.21ms
sample 10: path=regional_relay connect=263.54ms
summary: sent=10 received=10 loss=0.0% connect min/avg/max=254.13/789.41/5555.72ms
exec: runnervmm7ud1
### w a
sample 1: path=regional_relay connect=594.86ms
sample 2: path=regional_relay connect=301.66ms
sample 3: path=regional_relay connect=302.35ms
sample 4: path=regional_relay connect=299.41ms
sample 5: path=regional_relay connect=303.33ms
sample 6: path=regional_relay connect=338.86ms
sample 7: path=regional_relay connect=302.80ms
sample 8: path=regional_relay connect=300.63ms
sample 9: path=regional_relay connect=303.37ms
sample 10: path=regional_relay connect=301.67ms
summary: sent=10 received=10 loss=0.0% connect min/avg/max=299.41/334.89/594.86ms
exec: 609f8b344982
## doctor a>w while blocked
Paperboat doctor: unhealthy
selected_path: warning: The active Paperboat path uses WebSocket fallback.
## rule removed 22:42:41Z
sample 1: path=direct connect=283.89ms
sample 6: path=direct connect=227.08ms
summary: sent=6 received=6 loss=0.0% connect min/avg/max=225.75/236.82/283.89ms
sample 1: path=direct connect=256.16ms
sample 6: path=direct connect=228.20ms
summary: sent=6 received=6 loss=0.0% connect min/avg/max=227.05/233.37/256.16ms
sample 1: path=direct connect=225.03ms
sample 6: path=direct connect=230.40ms
summary: sent=6 received=6 loss=0.0% connect min/avg/max=225.03/227.59/230.40ms
```

## UDP blocked on M (pf, same rule): PASS M<->W; M<->L is relay-only in this topology (behind two NATs), so it stays relay
```
## UDP block on M (rx38n-m, pf) 22:48:17Z
ALTQ related functions disabled
pfctl: pf already enabled
No ALTQ support in kernel
ALTQ related functions disabled
block drop quick proto udp from any to any port != 53
block drop quick proto udp from any port != 53 to any
### m w
sample 1: path=regional_relay connect=5460.18ms
sample 2: path=regional_relay connect=480.57ms
sample 8: path=regional_relay connect=955.75ms
summary: sent=8 received=8 loss=0.0% connect min/avg/max=480.57/1164.36/5460.18ms
exec: runnervmm7ud1
### w m
sample 1: path=regional_relay connect=4182.05ms
sample 2: path=regional_relay connect=492.21ms
sample 8: path=regional_relay connect=486.86ms
summary: sent=8 received=8 loss=0.0% connect min/avg/max=486.44/949.56/4182.05ms
exec: sat12-bq155-796eb898-74a7-4e61-aa5b-1b691cd4876e-5A2EC5B81174.local
### m a
sample 1: path=regional_relay connect=908.92ms
sample 2: path=regional_relay connect=288.19ms
sample 8: path=regional_relay connect=288.85ms
summary: sent=8 received=8 loss=0.0% connect min/avg/max=285.48/495.12/1322.54ms
exec: 609f8b344982
### a m
sample 1: path=regional_relay connect=856.83ms
sample 2: path=regional_relay connect=288.78ms
sample 8: path=regional_relay connect=288.76ms
summary: sent=8 received=8 loss=0.0% connect min/avg/max=286.51/359.20/856.83ms
exec: sat12-bq155-796eb898-74a7-4e61-aa5b-1b691cd4876e-5A2EC5B81174.local
## doctor m>w
Paperboat doctor: unhealthy
selected_path: unavailable: No active Paperboat path is currently observed.
pf disabled
## pf disabled 22:49:34Z
sample 1: path=direct connect=77.65ms
sample 6: path=direct connect=33.64ms
summary: sent=6 received=6 loss=0.0% connect min/avg/max=33.41/41.36/77.65ms
sample 1: path=direct connect=39.52ms
sample 6: path=direct connect=40.40ms
summary: sent=6 received=6 loss=0.0% connect min/avg/max=30.63/40.06/63.67ms
sample 1: path=regional_relay connect=3951.62ms
sample 6: path=regional_relay connect=3739.37ms
summary: sent=6 received=6 loss=0.0% connect min/avg/max=494.04/1613.51/3951.62ms
sample 1: path=regional_relay connect=476.18ms
sample 6: path=regional_relay connect=478.62ms
summary: sent=6 received=6 loss=0.0% connect min/avg/max=476.18/515.45/701.70ms
```

## UDP blocked on W with the Windows Firewall: not a valid bed (finding)
```
## UDP block on W (rx38n-w, Defender Firewall rules out+in) 22:42:59Z
2
### a w
sample 1: path=direct connect=256.32ms
sample 2: path=direct connect=227.94ms
sample 10: path=direct connect=230.25ms
summary: sent=10 received=10 loss=0.0% connect min/avg/max=226.62/231.18/256.32ms
exec: runnervmm7ud1
### w a
sample 1: path=regional_relay connect=796.75ms
sample 2: path=regional_relay connect=451.44ms
sample 10: path=regional_relay connect=293.34ms
summary: sent=10 received=10 loss=0.0% connect min/avg/max=292.79/359.59/796.75ms
exec: 609f8b344982
### m w
sample 1: path=regional_relay connect=1198.33ms
sample 2: path=regional_relay connect=469.67ms
sample 10: path=regional_relay connect=467.20ms
summary: sent=10 received=10 loss=0.0% connect min/avg/max=467.20/568.67/1198.33ms
exec: runnervmm7ud1
### w m
sample 1: timeout
sample 2: path=regional_relay connect=713.77ms
sample 10: path=regional_relay connect=522.63ms
summary: sent=10 received=9 loss=10.0% connect min/avg/max=522.63/575.65/790.13ms
exec: sat12-bq155-796eb898-74a7-4e61-aa5b-1b691cd4876e-5A2EC5B81174.local
## doctor w>m
Paperboat doctor: degraded
selected_path: warning: The active Paperboat path uses a regional relay.
0
## rule removed 22:44:21Z
sample 1: path=direct connect=255.46ms
sample 6: path=direct connect=226.98ms
summary: sent=6 received=6 loss=0.0% connect min/avg/max=223.77/231.16/255.46ms
sample 1: path=direct connect=227.23ms
sample 6: path=direct connect=228.33ms
summary: sent=6 received=6 loss=0.0% connect min/avg/max=225.64/228.02/230.31ms
sample 1: path=direct connect=63.83ms
sample 6: path=direct connect=31.74ms
summary: sent=6 received=6 loss=0.0% connect min/avg/max=30.59/36.75/63.83ms
sample 1: path=direct connect=31.05ms
sample 6: path=direct connect=32.79ms
summary: sent=6 received=6 loss=0.0% connect min/avg/max=30.83/31.52/32.79ms
DisplayName  Enabled Direction Action
-----------  ------- --------- ------
x38n-udp-out    True  Outbound  Block
x38n-udp-in     True   Inbound  Block
Name    Enabled DefaultInboundAction
----    ------- --------------------
Domain     True        NotConfigured
Private    True        NotConfigured
Public     True        NotConfigured
blocked at 22:44:51Z; waiting 75 s
Paperboat daemon service restarted and ready
22:46:14
### a w (daemon restarted under block)
sample 1: path=direct connect=344.87ms
sample 2: path=direct connect=298.35ms
sample 8: path=direct connect=250.05ms
summary: sent=8 received=8 loss=0.0% connect min/avg/max=246.11/274.78/344.87ms
exec: runnervmm7ud1
### w a (daemon restarted under block)
sample 1: path=regional_relay connect=4789.95ms
sample 2: path=regional_relay connect=294.82ms
sample 8: path=regional_relay connect=303.09ms
summary: sent=8 received=8 loss=0.0% connect min/avg/max=264.32/847.09/4789.95ms
exec: 609f8b344982
### m w (daemon restarted under block)
sample 1: path=regional_relay connect=1810.24ms
sample 2: path=regional_relay connect=741.48ms
sample 8: path=regional_relay connect=480.53ms
summary: sent=8 received=8 loss=0.0% connect min/avg/max=479.81/681.06/1810.24ms
exec: runnervmm7ud1
### w m (daemon restarted under block)
sample 1: path=regional_relay connect=2995.97ms
sample 2: path=regional_relay connect=499.51ms
sample 8: path=regional_relay connect=501.00ms
summary: sent=8 received=8 loss=0.0% connect min/avg/max=498.66/842.22/2995.97ms
exec: sat12-bq155-796eb898-74a7-4e61-aa5b-1b691cd4876e-5A2EC5B81174.local
## W block, program-scoped (pb.exe both paths) 22:50:39Z
New-NetFirewallRule : Cannot process argument transformation on parameter 'Program'. Cannot convert value to type 
System.String.
At line:1 char:395
+ ... n $d -Protocol UDP -RemotePort "1-52","54-65535" -Program $ps -Action ...
+                                                               ~~~
    + CategoryInfo          : InvalidData: (:) [New-NetFirewallRule], ParameterBindingArgumentTransformationException
    + FullyQualifiedErrorId : ParameterArgumentTransformationError,New-NetFirewallRule
 
New-NetFirewallRule : Cannot process argument transformation on parameter 'Program'. Cannot convert value to type 
System.String.
At line:1 char:395
+ ... n $d -Protocol UDP -RemotePort "1-52","54-65535" -Program $ps -Action ...
+                                                               ~~~
    + CategoryInfo          : InvalidData: (:) [New-NetFirewallRule], ParameterBindingArgumentTransformationException
    + FullyQualifiedErrorId : ParameterArgumentTransformationError,New-NetFirewallRule
 
0
Paperboat daemon service restarted and ready
### a w
sample 1: path=direct connect=484.58ms
sample 2: path=direct connect=230.00ms
sample 10: path=direct connect=227.85ms
sample 11: path=direct connect=228.31ms
sample 12: path=direct connect=228.26ms
sample 13: path=direct connect=227.03ms
sample 14: path=direct connect=230.22ms
sample 15: path=direct connect=227.71ms
sample 16: path=direct connect=225.86ms
sample 17: path=direct connect=228.01ms
sample 18: path=direct connect=228.62ms
sample 19: path=direct connect=227.57ms
sample 20: path=direct connect=227.65ms
summary: sent=20 received=20 loss=0.0% connect min/avg/max=225.86/240.79/484.58ms
### w a
sample 1: path=regional_relay connect=1971.17ms
sample 2: path=direct connect=259.43ms transition
sample 10: path=direct connect=216.46ms
sample 11: path=direct connect=224.73ms
sample 12: path=direct connect=216.15ms
sample 13: path=direct connect=217.35ms
sample 14: path=direct connect=216.57ms
sample 15: path=direct connect=214.45ms
sample 16: path=direct connect=215.91ms
sample 17: path=direct connect=428.77ms
sample 18: path=direct connect=216.72ms
sample 19: path=direct connect=219.31ms
sample 20: path=direct connect=216.58ms
summary: sent=20 received=20 loss=0.0% connect min/avg/max=214.45/347.60/1971.17ms
## W block, program-scoped 22:51:18Z
4
Paperboat daemon service restarted and ready
### a w
sample 1: path=direct connect=276.51ms
sample 2: path=direct connect=224.82ms
sample 10: path=direct connect=231.47ms
sample 11: path=direct connect=230.21ms
sample 12: path=direct connect=229.18ms
sample 13: path=direct connect=228.60ms
sample 14: path=direct connect=227.86ms
sample 15: path=direct connect=230.76ms
sample 16: path=direct connect=228.06ms
sample 17: path=direct connect=230.35ms
sample 18: path=direct connect=228.73ms
sample 19: path=direct connect=237.06ms
sample 20: path=direct connect=228.09ms
summary: sent=20 received=20 loss=0.0% connect min/avg/max=224.82/231.35/276.51ms
### w a
sample 1: path=regional_relay connect=4558.60ms
sample 2: path=regional_relay connect=266.39ms
sample 10: path=regional_relay connect=265.91ms
sample 11: path=regional_relay connect=266.42ms
sample 12: path=regional_relay connect=265.76ms
sample 13: path=regional_relay connect=265.43ms
sample 14: path=regional_relay connect=270.30ms
sample 15: path=regional_relay connect=264.19ms
sample 16: path=regional_relay connect=265.22ms
sample 17: path=regional_relay connect=267.70ms
sample 18: path=regional_relay connect=263.00ms
sample 19: path=regional_relay connect=497.61ms
sample 20: path=regional_relay connect=265.78ms
summary: sent=20 received=20 loss=0.0% connect min/avg/max=263.00/494.11/4558.60ms
## FINDING: a Windows Firewall UDP block (generic or pb.exe-scoped, inbound+outbound, daemon restarted) does not stop the direct path: L>W stays path=direct, and tcpdump on the L host shows 337 UDP packets from W's address in 20 s during the ping, although w32tm UDP 123 from W is dropped by the same outbound rule. W>L prints regional_relay and W's doctor says WebSocket fallback. A host firewall on the Windows end is therefore not a valid "UDP blocked" bed (existing allow rule "Paperboat pb.exe" + stateful return traffic); the UDP-blocked proof for L<->W uses the block on the L end, for M<->W the pf block on the M end.
```

## Network change in the middle of a session: PASS on L, M and W (a long exec survives, path direct again, doctor path check pass)
L: container moved between docker bridges (172.17.0.6 -> 172.38.8.3) during a 60 s exec; M: network service Ethernet disabled 15 s (20 s without network) during a 60 s exec; W: adapter 'Ethernet 3' (default route) disabled 15 s during a 61 s exec. Wi-Fi switch equivalent: new address on L, interface down/up on M and W.
```
## Network change on L: long exec a>w, rx38n-a moves bridge->x38n (new IPv4) mid-session 23:09:29Z
baseline: sample 2: path=direct connect=232.84ms
exec start 23:09:32
EXEC-FINISHED
exec end rc=0 23:10:36
t+20 23:09:52 moving rx38n-a: bridge -> x38n
172.17.0.6 
eth0@if300       UP             172.38.8.3/24 
default via 172.38.8.1 dev eth0 
172.38.8.0/24 dev eth0 proto kernel scope link src 172.38.8.3 
t+ 23:09:53 moved
sample 1: path=direct connect=271.19ms
sample 5: path=direct connect=238.05ms
summary: sent=5 received=5 loss=0.0% connect min/avg/max=238.05/245.66/271.19ms
sample 1: path=direct connect=240.43ms
sample 5: path=direct connect=237.06ms
summary: sent=5 received=5 loss=0.0% connect min/avg/max=237.06/238.50/240.43ms
sample 1: path=direct connect=264.97ms
sample 5: path=direct connect=242.06ms
summary: sent=5 received=5 loss=0.0% connect min/avg/max=236.09/244.06/264.97ms
Paperboat doctor: healthy
peer_paths: pass: Paths: rx38n-w direct.
609f8b344982

## Network change on M: service Ethernet off 15 s then on, during a long exec w>m 23:11:14Z
baseline: sample 2: path=direct connect=232.01ms transition
netcut armed 23:11:22
exec start 23:11:22
EXEC-FINISHED
exec end 23:12:35
cut 23:11:37 192.168.64.3
up 23:11:57 192.168.64.3
192.168.64.3
sample 1: path=direct connect=61.42ms
sample 5: path=direct connect=44.99ms
summary: sent=5 received=5 loss=0.0% connect min/avg/max=35.33/45.23/61.42ms
sample 1: path=direct connect=30.65ms
sample 5: path=direct connect=33.63ms
summary: sent=5 received=5 loss=0.0% connect min/avg/max=30.65/32.38/33.63ms
sample 1: path=direct connect=60.57ms
sample 5: path=direct connect=30.48ms
summary: sent=5 received=5 loss=0.0% connect min/avg/max=30.45/36.92/60.57ms
Paperboat doctor: unhealthy
peer_paths: pass: No connection to another machine is in use.
runnervmm7ud1
### a > m  23:13:06Z
summary: sent=4 received=4 loss=0.0% connect min/avg/max=248.30/384.61/783.27ms
exec: sat12-bq155-796eb898-74a7-4e61-aa5b-1b691cd4876e-5A2EC5B81174.local

## Network change on W: adapter Ethernet 3 (default route) disabled 15 s then enabled, during a long exec m>w 23:15:27Z
baseline: sample 2: path=direct connect=232.37ms transition
          0
exec start 23:15:35
/bin/bash: %: command not found
/bin/bash: EXEC-FINISHED: command not found
exec end 23:15:41
## (redo, exec in flight) Network change on W: Ethernet 3 disabled 15 s then enabled 23:18:06Z
cut 23:15:51
up 23:16:08
(previous log above)
          0
exec start 23:18:12
Ping statistics for 127.0.0.1:
    Packets: Sent = 61, Received = 61, Lost = 0 (0% loss),
Approximate round trip times in milli-seconds:
    Minimum = 0ms, Maximum = 0ms, Average = 0ms
exec end 23:19:18
Connection closed by UNKNOWN port 65535
sample 1: path=direct connect=60.80ms
sample 5: path=direct connect=32.18ms
summary: sent=5 received=5 loss=0.0% connect min/avg/max=30.25/38.02/60.80ms
sample 1: path=direct connect=31.19ms
sample 5: path=direct connect=30.45ms
summary: sent=5 received=5 loss=0.0% connect min/avg/max=30.43/30.75/31.21ms
Paperboat doctor: degraded
authentication: pass: The Paperboat server accepted the current credential.
peer_paths: pass: No connection to another machine is in use.
### a > w  23:20:34Z
summary: sent=4 received=4 loss=0.0% connect min/avg/max=230.65/356.40/732.42ms
exec: runnervmm7ud1
    + FullyQualifiedErrorId : NativeCommandError
```

## Daemon-level proxy (the daemon's environment is not the shell's)
L: PAPERBOAT_HTTPS_PROXY in the user service manager, proxy log shows CONNECT api.pprbt.dev:443 (honoured); dead proxy: daemon 'not ready (degraded): Control plane is unavailable. Check network access and Paperboat authentication.' and pb doctor with a dead HTTPS_PROXY in the shell says 'did not accept the authenticated health check' (neither names the proxy: fixed in 69feaf132, see below). A shell-only HTTPS_PROXY is not seen by the daemon (it keeps working).
M: macOS system proxy (networksetup secure web proxy, scutil --proxy HTTPSEnable 1) is honoured by the daemon (CONNECT api.pprbt.dev:443 x9 in the proxy log) and exec/ping keep working; removed again.
W: machine environment PAPERBOAT_HTTPS_PROXY honoured (6 x CONNECT api.pprbt.dev:443). WinINET (HKCU ProxyEnable/ProxyServer) and WinHTTP (netsh winhttp set proxy) are IGNORED by dev-ef42e84a9 and nothing names that (fixed in 69feaf132, verification below).
```
## L daemon-level proxy: PAPERBOAT_HTTPS_PROXY in the user service manager 22:56:36Z
Paperboat daemon service restarted and ready
bash: line 1: /proc/1077/environ: Permission denied
pid 3103 PAPERBOAT_HTTPS_PROXY=http://127.0.0.1:3128
bash: line 1: /proc/1304/environ: Permission denied
bash: line 1: /proc/1662/environ: Permission denied
Paperboat doctor: healthy
authentication: pass: The Paperboat server accepted the current credential.
machine_credentials: pass: The Paperboat server accepts this machine's credentials.
-- proxy log (CONNECT targets):
      5 CONNECT api.pprbt.dev:443
### a > w  22:56:50Z
summary: sent=4 received=4 loss=0.0% connect min/avg/max=230.85/483.02/1014.23ms
exec: runnervmm7ud1
## proxy killed, daemon keeps PAPERBOAT_HTTPS_PROXY 22:57:07Z
-- doctor (CLI process; env of the shell has no proxy):
Paperboat doctor: healthy
authentication: pass: The Paperboat server accepted the current credential.
-- daemon restart under dead proxy, then status:
pb: The Paperboat local daemon is running but not ready (degraded): Control plane is unavailable. Check network access and Paperboat authentication. Support reference: support_e0e3f49e-d8aa-409e-88ac-5e29622693e6. Run `pb bugreport --upload` to send the details.
Paperboat doctor: unhealthy
authentication: pass: The Paperboat server accepted the current credential.
clock: pass: This machine's clock matches the Paperboat server's.
  Recovery: Check network access and Paperboat authentication.
  Recovery: Check network access and Paperboat authentication
### a > w  22:57:45Z
exec: pb: The target machine was reached but did not open the connection. Its Paperboat service may still be starting, or it has not accepted this machine yet. Retry in a minute; run `pb doctor` if this continues. Support reference: support_88ad081b-aa35-42c7-8337-40a47415add0. Run `pb bugreport --upload` to send the details.
## shell-only HTTPS_PROXY=dead (daemon has none): doctor and exec
Paperboat daemon service restarted and ready
Paperboat doctor: unhealthy
authentication: fail: The Paperboat server did not accept the authenticated health check.
  Recovery: Check network access and sign in again if needed, then run pb doctor again.
  Recovery: Check the connection to Paperboat and run `pb doctor` again.
Paperboat doctor: healthy
authentication: pass: The Paperboat server accepted the current credential.
### a > w  22:57:59Z
summary: sent=4 received=4 loss=0.0% connect min/avg/max=232.88/487.70/1013.93ms
exec: runnervmm7ud1

## M system proxy (networksetup secure web proxy) 22:58:51Z
  HTTPSEnable : 1
  HTTPSPort : 3128
  HTTPSProxy : 127.0.0.1
  ProxyAutoConfigEnable : 1
pb: The Paperboat local daemon is running but not ready (starting): Local machine access is unavailable: the guard connect step failed (not_found). Check Paperboat local access settings and repair the installation. Support reference: support_3efe81ae-14e0-49e9-830b-6276058cd1ae. Run `pb bugreport --upload` to send the details.
Paperboat doctor: unhealthy
authentication: pass: The Paperboat server accepted the current credential.
machine_credentials: pass: The Paperboat server accepts this machine's credentials.
-- proxy log:
   9 CONNECT api.pprbt.dev:443
### m > w  22:59:36Z
summary: sent=4 received=4 loss=0.0% connect min/avg/max=31.20/723.08/2591.02ms
exec: runnervmm7ud1
### a > m  22:59:49Z
summary: sent=4 received=4 loss=0.0% connect min/avg/max=291.70/700.65/1912.47ms
exec: sat12-bq155-796eb898-74a7-4e61-aa5b-1b691cd4876e-5A2EC5B81174.local
-- doctor again (same proxy, running) 23:00:05Z
Paperboat doctor: unhealthy
authentication: pass: The Paperboat server accepted the current credential.
daemon: pending: The local daemon is starting. Local machine access is unavailable: the guard connect step failed (not_found).
  Recovery: Paperboat is still starting; run `pb doctor` again in a moment.
daemon:local_access_unavailable: fail: Local machine access is unavailable: the guard connect step failed (not_found)
  Recovery: Check Paperboat local access settings and repair the installation
daemon:machine_services: fail: Reading the services your machines announce for the browser failed: local machine access reconciliation failed; retrying in 11s
  Recovery: Paperboat retries this on its own. If it keeps failing, check this computer's network and that the Paperboat server is reachable, then run `pb doctor` again.
## proxy killed, system proxy still set
Paperboat doctor: unhealthy
authentication: fail: The Paperboat server did not accept the authenticated health check.
  Recovery: Check network access and sign in again if needed, then run pb doctor again.
  Recovery: Check network access and Paperboat authentication.
  Recovery: Check network access and Paperboat authentication
pb: The Paperboat local daemon is running but not ready (degraded): Control plane is unavailable. Check network access and Paperboat authentication. Support reference: support_b1be2e06-5c6a-4b80-b4f9-
## system proxy removed + restart
0
pb: The Paperboat local daemon is running but not ready (starting): Local machine access is unavailable: the guard connect step failed (not_found). Check Paperboat local access settings and repair the
Paperboat doctor: unhealthy
authentication: pass: The Paperboat server accepted the current credential.
### m > w  23:01:30Z
summary: sent=4 received=4 loss=0.0% connect min/avg/max=30.75/740.87/2668.02ms
exec: runnervmm7ud1

## W: python + proxy up 23:03:02Z
1
    Direct access (no proxy server).
0
## A: WinINET only (HKCU ProxyEnable=1 ProxyServer=127.0.0.1:3128)
Paperboat daemon service restarted and ready
Paperboat doctor: degraded
authentication: pass: The Paperboat server accepted the current credential.
proxy log lines: 0
## B: WinHTTP machine proxy only (netsh winhttp set proxy), WinINET off
    Bypass List     :  (none)
Paperboat daemon service restarted and ready
Paperboat doctor: degraded
authentication: pass: The Paperboat server accepted the current credential.
proxy log: ## C: machine environment PAPERBOAT_HTTPS_PROXY (service env), WinHTTP reset
    + FullyQualifiedErrorId : NativeCommandError
Paperboat doctor: unhealthy
authentication: fail: Paperboat authentication is unavailable.
  Recovery: Check network access and Paperboat authentication.
  Recovery: Check network access and Paperboat authentication
proxy log: ## C (redo): proxy alive via CIM process, machine env PAPERBOAT_HTTPS_PROXY set 23:08:17Z
ReturnValue
-----------
          0
1
http://127.0.0.1:3128
Paperboat daemon service restarted and ready
Paperboat doctor: degraded
authentication: pass: The Paperboat server accepted the current credential.
proxy log:
6 CONNECT api.pprbt.dev:443
### w > a  23:08:40Z
summary: sent=4 received=4 loss=0.0% connect min/avg/max=249.73/685.08/1984.63ms
exec: 
```

## macOS machine guard and PF (finding)
On rx38n-m the guard (io.paperboat.machineguard) crash-looped (exit 1, 290 runs in 30 min, only the generic 'Paperboat could not finish the command' in /var/log/io.paperboat.machineguard.log) from the first install until `pb install` was run again, and again after PF was disabled (pfctl -d); it stays up when PF is enabled and the daemon is restarted. Local access then says 'guard connect step failed (not_found)', doctor unhealthy. Run by hand the guard prints the same generic line, so the cause is not readable.
```
## baseline 23:07:08Z
	runs = 25
Status: Enabled for 0 days 00:01:21           Debug: Urgent
scrub-anchor "com.apple/*" all fragment reassemble
anchor "com.apple/*" all
  com.apple
## load user ruleset (pfctl -f block-udp -e)
pfctl: pf already enabled
	state = running
	runs = 25
		state = active
		state = active
Paperboat doctor: healthy
machine_guard_version: pass: The machine guard runs this pb's version.
## flush+disable (pfctl -F all; -d)
pf: interface flags reset
pf disabled
	state = running
	runs = 25
		state = active
		state = active
Paperboat doctor: healthy
machine_guard_version: pass: The machine guard runs this pb's version.
## restore default pf ruleset and enable pf, do not touch the guard 23:13:41Z
pfctl: Use of -f option, could result in flushing of rules
present in the main ruleset added by the system at startup.

pf enabled
Status: Enabled for 0 days 00:00:00           Debug: Urgent
	state = running
	runs = 154
	last exit code = 1
		state = active
		state = active
71446	1	io.paperboat.machineguard
Paperboat doctor: unhealthy
daemon:local_access_unavailable: fail: Local machine access is unavailable: the guard connect step failed (not_found)
machine_guard_version: pass: The machine guard runs this pb's version.
23:14:18
	state = running
	runs = 154
		state = active
		state = active
Paperboat doctor: unhealthy
daemon:local_access_unavailable: fail: Local machine access is unavailable: the guard connect step failed (not_found)
machine_guard_version: pass: The machine guard runs this pb's version.
Paperboat daemon service restarted and ready
Paperboat doctor: healthy
```

## Full-tunnel VPN coexistence: the user's own devices run on a Tailscale tailnet (scope decision), recorded
victus (Windows, fleet lanex2-w, real PC): tailscale BackendState=Running, 12 tailnet peers, default route via the External Virtual Switch, pb doctor healthy; oracle: tailscale BackendState=Running, default route enp0s6. Earlier WireGuard default-route passes: part 2 (Linux and Windows).

## Captive portal (names of the control plane and both edges answered by a 302 / self-signed HTTPS portal for 150 s via the hosts file): recovers by itself on M and W (build dev-ef42e84a9)
Direct path and exec keep working while the portal is on; doctor authentication fails ('did not accept the authenticated health check', clock unreachable; it does not name a captive portal); 2-8 s after the hosts entries are removed authentication passes again with no daemon restart. M doctor stayed unhealthy afterwards only because of the macOS guard/pf finding above (a custom pf root ruleset was loaded earlier in that run); W doctor is 'degraded' before and after (HTTP.sys holds ports 80/443 on the runner, a runner property).
```
## Captive portal in front of M: api and edge names answered by a 302 / self-signed HTTPS portal for 150 s 23:30:40Z
baseline: sample 2: path=direct connect=33.64ms ; Paperboat doctor: healthy
--- 23:30:50 (portal on)
sample 1: path=direct connect=68.00ms
sample 2: path=direct connect=46.98ms
sample 3: path=direct connect=40.82ms
summary: sent=3 received=3 loss=0.0% connect min/avg/max=40.82/51.93/68.00ms
Paperboat doctor: unhealthy
authentication: fail: The Paperboat server did not accept the authenticated health check.
clock: warning: The Paperboat server could not be reached to read its clock.
sat12-bq155-796eb898-74a7-4e61-aa5b-1b691cd4876e-5A2EC5B81174.local
--- 23:31:05 (portal on)
sample 1: path=direct connect=62.06ms
sample 2: path=direct connect=34.20ms
sample 3: path=direct connect=35.95ms
summary: sent=3 received=3 loss=0.0% connect min/avg/max=34.20/44.07/62.06ms
Paperboat doctor: unhealthy
authentication: fail: The Paperboat server did not accept the authenticated health check.
clock: warning: The Paperboat server could not be reached to read its clock.
sat12-bq155-796eb898-74a7-4e61-aa5b-1b691cd4876e-5A2EC5B81174.local
--- 23:31:21 (portal on)
sample 1: path=direct connect=62.78ms
sample 2: path=direct connect=36.84ms
sample 3: path=direct connect=33.25ms
summary: sent=3 received=3 loss=0.0% connect min/avg/max=33.25/44.29/62.78ms
Paperboat doctor: unhealthy
authentication: fail: The Paperboat server did not accept the authenticated health check.
clock: warning: The Paperboat server could not be reached to read its clock.
sat12-bq155-796eb898-74a7-4e61-aa5b-1b691cd4876e-5A2EC5B81174.local
--- waiting with pings 23:31:40
23:31:43
portal ON 23:30:52
/bin/bash: /Users/runner/x38portal/portal.log: No such file or directory
cut: /Users/runner/x38portal/portal.log: No such file or directory
--- 23:32:10 sample 2: path=direct connect=33.80ms | Paperboat doctor: unhealthy authentication: fail: The Paperboat server did not accept the authenticated health
--- 23:32:37 sample 2: path=direct connect=31.57ms | Paperboat doctor: unhealthy authentication: fail: The Paperboat server did not accept the authenticated health
--- 23:33:04 sample 2: path=direct connect=31.61ms | Paperboat doctor: unhealthy authentication: fail: The Paperboat server did not accept the authenticated health
--- 23:33:30 sample 2: path=direct connect=30.83ms | Paperboat doctor: unhealthy authentication: pass: The Paperboat server accepted the current credential. 
--- 23:33:57 sample 2: path=direct connect=35.77ms | Paperboat doctor: unhealthy authentication: pass: The Paperboat server accepted the current credential. 
--- 23:34:24 sample 2: path=direct connect=31.15ms | Paperboat doctor: unhealthy authentication: pass: The Paperboat server accepted the current credential. 
--- 23:34:51 sample 2: path=direct connect=31.60ms | Paperboat doctor: unhealthy authentication: pass: The Paperboat server accepted the current credential. 
--- 23:35:18 sample 2: path=direct connect=240.56ms transition | Paperboat doctor: healthy authentication: pass: The Paperboat server accepted the current credential. 
--- 23:35:49 sample 2: path=direct connect=58.71ms | Paperboat doctor: healthy authentication: pass: The Paperboat server accepted the current credential. 
portal ON 23:30:52
portal OFF 23:33:22

## Captive portal in front of W: api and edge names answered by a 302 / self-signed HTTPS portal for 150 s 23:37:38Z (build dev-ef42e84a9)
baseline: sample 2: path=direct connect=30.52ms ; w doctor: Paperboat doctor: degraded authentication: pass: The Paperboat server accepted the current credential. 
          0
--- 23:37:48 (portal on)
summary: sent=3 received=3 loss=0.0% connect min/avg/max=30.61/40.01/58.78ms
Paperboat doctor: degraded
authentication: pass: The Paperboat server accepted the current credential.
clock: pass: This machine's clock matches the Paperboat server's.
runnervmm7ud1
--- 23:37:59 (portal on)
summary: sent=3 received=3 loss=0.0% connect min/avg/max=30.70/30.95/31.36ms
Paperboat doctor: unhealthy
authentication: fail: The Paperboat server did not accept the authenticated health check.
clock: warning: The Paperboat server could not be reached to read its clock.
runnervmm7ud1
--- 23:38:17 sample 2: path=direct connect=32.06ms | Paperboat doctor: unhealthy authentication: fail: The Paperboat server did not accept the authenticated health check. 
--- 23:38:39 sample 2: path=direct connect=31.18ms | Paperboat doctor: unhealthy authentication: fail: The Paperboat server did not accept the authenticated health check. 
--- 23:39:01 sample 2: path=direct connect=31.42ms | Paperboat doctor: unhealthy authentication: fail: The Paperboat server did not accept the authenticated health check. 
--- 23:39:24 sample 2: path=direct connect=31.01ms | Paperboat doctor: unhealthy authentication: fail: The Paperboat server did not accept the authenticated health check. 
--- 23:39:45 sample 2: path=direct connect=30.49ms | Paperboat doctor: unhealthy authentication: fail: The Paperboat server did not accept the authenticated health check. 
--- 23:40:07 sample 2: path=direct connect=30.91ms | Paperboat doctor: unhealthy authentication: fail: The Paperboat server did not accept the authenticated health check. 
--- 23:40:28 sample 2: path=direct connect=30.11ms | Paperboat doctor: degraded authentication: pass: The Paperboat server accepted the current credential. 
--- 23:40:50 sample 2: path=direct connect=30.07ms | Paperboat doctor: degraded authentication: pass: The Paperboat server accepted the current credential. 
--- 23:41:11 sample 2: path=direct connect=31.37ms | Paperboat doctor: degraded authentication: pass: The Paperboat server accepted the current credential. 
portal ON 23:37:56
portal OFF 23:40:26
```

## Host firewalls on L (default policies): direct both ways, exec both ways: PASS (build dev-ef42e84a9)
ufw (default deny incoming) first shows regional_relay for under a minute while the path is punched again, then direct 4 x 40 samples; firewalld default zone (nft input policy drop; its D-Bus client hangs in a container, the installed ruleset is what is tested); nftables input policy drop with established/related, lo and tcp 22 only.
```
## ufw default (deny incoming, allow outgoing) on L (build dev-ef42e84a9) 23:38:26Z
Firewall is active and enabled on system startup
Status: active
Logging: on (low)
Default: deny (incoming), allow (outgoing), deny (routed)
New profiles: skip
### a > w  23:38:28Z
sample 4: path=regional_relay connect=265.00ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=262.52/523.32/1028.16ms
exec: runnervmm7ud1
### w > a  23:38:35Z
sample 4: path=regional_relay connect=268.14ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=264.49/391.34/766.33ms
exec: 
### a > m  23:38:47Z
sample 4: path=regional_relay connect=291.26ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=286.21/695.27/1901.85ms
exec: sat12-bq155-796eb898-74a7-4e61-aa5b-1b691cd4876e-5A2EC5B81174.local
### m > a  23:38:55Z
sample 4: path=regional_relay connect=282.72ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=282.36/740.60/2107.32ms
exec: 609f8b344982
### a > v6  23:39:09Z
sample 4: path=direct connect=5.79ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=4.43/29.79/102.52ms
exec: 343406103e99
### v6 > a  23:39:15Z
sample 4: path=direct connect=4.84ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=4.84/51.06/179.22ms
exec: 609f8b344982
Firewall stopped and disabled on system startup
## ufw enabled, wait for hole punching 23:39:29Z
Firewall is active and enabled on system startup
sample 1: path=direct connect=264.69ms
sample 40: path=direct connect=231.07ms
summary: sent=40 received=40 loss=0.0% connect min/avg/max=225.81/231.65/264.69ms
23:39:43
sample 1: path=direct connect=257.39ms
sample 40: path=direct connect=229.47ms
summary: sent=40 received=40 loss=0.0% connect min/avg/max=227.40/232.77/259.22ms
23:39:53
sample 1: path=direct connect=254.83ms
sample 40: path=direct connect=229.59ms
summary: sent=40 received=40 loss=0.0% connect min/avg/max=225.25/231.85/257.17ms
23:40:04
sample 1: path=direct connect=257.54ms
sample 40: path=direct connect=227.00ms
summary: sent=40 received=40 loss=0.0% connect min/avg/max=225.38/232.39/282.11ms
23:40:15
### w > a  23:40:15Z
sample 4: path=direct connect=229.50ms
exec: 
Firewall stopped and disabled on system startup
## firewalld default zone on L 23:40:38Z
ERROR:dbus.proxies:Introspect error on :1.29:/org/fedoraproject/FirewallD1: dbus.exceptions.DBusException: org.freedesktop.DBus.Error.NoReply: Did not receive a reply. Possible causes include: the remote application did not send a reply, the message bus security policy blocked the reply, the reply timeout expired, or the network connection was broken.
Error: Did not receive a reply. Possible causes include: the remote application did not send a reply, the message bus security policy blocked the reply, the reply timeout expired, or the network connection was broken.
ERROR:dbus.proxies:Introspect error on :1.29:/org/fedoraproject/FirewallD1: dbus.exceptions.DBusException: org.freedesktop.DBus.Error.NoReply: Did not receive a reply. Possible causes include: the remote application did not send a reply, the message bus security policy blocked the reply, the reply timeout expired, or the network connection was broken.
Error: Did not receive a reply. Possible causes include: the remote application did not send a reply, the message bus security policy blocked the reply, the reply timeout expired, or the network connection was broken.
ERROR:dbus.proxies:Introspect error on :1.29:/org/fedoraproject/FirewallD1: dbus.exceptions.DBusException: org.freedesktop.DBus.Error.NoReply: Did not receive a reply. Possible causes include: the remote application did not send a reply, the message bus security policy blocked the reply, the reply timeout expired, or the network connection was broken.
sample 1: path=direct connect=467.65ms
sample 60: path=direct connect=220.37ms
summary: sent=60 received=60 loss=0.0% connect min/avg/max=218.60/228.32/467.65ms
### a > w  23:43:16Z
sample 4: path=direct connect=225.00ms
exec: runnervmm7ud1
### w > a  23:43:23Z
sample 4: path=regional_relay connect=257.09ms
exec: 
stopped
## nftables input policy drop (established,related, lo, tcp 22) on L 23:43:36Z
1
(firewalld started; firewall-cmd's D-Bus client hangs in this container, the nft ruleset firewalld installed is active):
		type filter hook input priority filter; policy drop;
sample 1: path=direct connect=220.82ms
sample 60: path=direct connect=443.45ms
summary: sent=60 received=60 loss=0.0% connect min/avg/max=219.10/294.04/837.95ms
sample 1: path=direct connect=253.96ms
sample 30: path=direct connect=222.58ms
summary: sent=30 received=30 loss=0.0% connect min/avg/max=219.61/393.75/879.01ms
### a > w  23:43:57Z
sample 4: path=direct connect=446.24ms
exec: runnervmm7ud1
### a > w  23:44:01Z
sample 4: path=direct connect=218.96ms
exec: runnervmm7ud1
### w > a  23:44:05Z
sample 4: path=direct connect=237.45ms
exec: 
removed
### w > a  23:44:08Z
sample 4: path=direct connect=240.62ms
exec: 
0
## nftables input policy drop (established,related, lo, tcp 22) on L 23:44:21Z
1
sample 1: path=direct connect=249.56ms
sample 40: path=direct connect=222.39ms
summary: sent=40 received=40 loss=0.0% connect min/avg/max=219.70/224.64/270.57ms
### a > w  23:44:33Z
sample 4: path=direct connect=220.90ms
exec: runnervmm7ud1
### w > a  23:44:40Z
sample 4: path=direct connect=239.02ms
exec: 
removed
```

## Plain HTTPS_PROXY / http_proxy in a Linux service environment: honoured
```
## L: plain HTTPS_PROXY (and http_proxy) in the user service manager (not PAPERBOAT_*) 23:32:46Z
Paperboat daemon service restarted and ready
Paperboat doctor: healthy
authentication: pass: The Paperboat server accepted the current credential.
-- proxy log:
      5 CONNECT api.pprbt.dev:443
pb: The Paperboat local daemon is running but not ready (degraded): Control plane is unavailable. Check network access and Paperboat authentication. Support reference: support_a5d043ed-f38d-4b5b-8cc4-b724827a6220. Run `pb bugreport --upload` to send the details.
Paperboat doctor: unhealthy
authentication: pass: The Paperboat server accepted the current credential.
  Recovery: Check network access and Paperboat authentication.
  Recovery: Check network access and Paperboat authentication
0
Paperboat daemon service restarted and ready
Paperboat doctor: healthy
authentication: pass: The Paperboat server accepted the current credential.
```

## NAT rebinding on M (attempt): pf nat rule + state flush
pf did not translate local UDP (states keep their source ports), so no real mapping change was produced on M; the state flush mid-exec left the 61-packet exec complete and the path direct (it also made the guard exit, see the guard section: a custom root ruleset). Mapping change on M is produced by the network change check above (interface down/up rebinds every socket).
```
## NAT rebinding on M (pf nat rewrites every new UDP source port to 40000-45000 and the state table is flushed mid-session) 23:27:44Z
baseline: sample 2: path=direct connect=31.78ms
exec start 23:27:50
    Packets: Sent = 61, Received = 61, Lost = 0 (0% loss),
exec end 23:28:55
rebind 23:28:05
ALL udp 192.168.64.3:51016 -> 140.245.11.41:27446       MULTIPLE:MULTIPLE
ALL udp 192.168.64.3:50958 -> 152.67.0.60:27446       MULTIPLE:MULTIPLE
ALL udp 192.168.64.3:59058 <- 48.211.213.32:21506       MULTIPLE:MULTIPLE
ALL udp 192.168.64.3:61384 -> 152.67.0.60:27444       MULTIPLE:MULTIPLE
ALL udp 192.168.64.3:63142 <- 152.67.0.60:27446       MULTIPLE:MULTIPLE
ALL udp 192.168.64.3:54086 <- 140.245.11.41:27446       MULTIPLE:MULTIPLE
nat on en0 inet proto udp all -> (en0) port 40000:45000 round-robin
sample 1: path=direct connect=61.74ms
sample 5: path=direct connect=32.53ms
summary: sent=5 received=5 loss=0.0% connect min/avg/max=31.29/38.31/61.74ms
sample 1: path=direct connect=35.75ms
sample 5: path=direct connect=32.26ms
summary: sent=5 received=5 loss=0.0% connect min/avg/max=32.26/34.45/36.07ms
sample 1: path=direct connect=59.46ms
sample 5: path=direct connect=32.46ms
summary: sent=5 received=5 loss=0.0% connect min/avg/max=32.11/38.16/59.46ms
```

# Second pass on the fix build dev-69feaf132 (commit 4dc82e483 on main): L rx38n-a, rx38n-v6, M rx38n-mb (new macpool-3 runner, the first one expired), W rx38n-w

## Proxy fix: dead configured proxy named, Windows system proxy honoured
```
## L on dev-69feaf132: dead proxy is named 23:49:59Z (dev-69feaf132)
-- shell-only HTTPS_PROXY=dead, pb doctor:
Paperboat doctor: unhealthy
authentication: fail: The Paperboat server is unreachable through the configured proxy.
  Recovery: The configured proxy did not accept the connection. Start it, or remove HTTPS_PROXY (PAPERBOAT_HTTPS_PROXY for a managed service) and the system proxy setting, then retry `pb doctor`.
  Recovery: Check the connection to Paperboat and run `pb doctor` again.
-- daemon with PAPERBOAT_HTTPS_PROXY=dead:
pb: The Paperboat local daemon is running but not ready (degraded): Control plane is unavailable: the configured proxy did not accept the connection. Start the proxy, or remove PAPERBOAT_HTTPS_PROXY (service environment) and the system proxy setting, then restart the daemon. Support reference: suppo
Paperboat doctor: unhealthy
daemon: warning: The local daemon is degraded. Control plane is unavailable: the configured proxy did not accept the connection.
  Recovery: Start the proxy, or remove PAPERBOAT_HTTPS_PROXY (service environment) and the system proxy setting, then restart the daemon.
daemon:control_plane_unavailable: fail: Control plane is unavailable: the configured proxy did not accept the connection
-- removed:
Paperboat daemon service restarted and ready
Paperboat doctor: healthy

## W on dev-69feaf132: system proxy honoured 23:50:53Z (dev-69feaf132)
-- A: WinINET (HKCU ProxyEnable=1 ProxyServer=127.0.0.1:3128), proxy alive
    + FullyQualifiedErrorId : NativeCommandError
Paperboat doctor: unhealthy
authentication: fail: The Paperboat server is unreachable through the configured proxy.
proxy log:
no log
### w > a  23:51:42Z
exec: 
-- WinINET dead (proxy stopped), pb doctor names it:
Paperboat doctor: unhealthy
authentication: fail: The Paperboat server is unreachable through the configured proxy.
  Recovery: The configured proxy did not accept the connection. Start it, or remove HTTPS_PROXY (PAPERBOAT_HTTPS_PROXY for a managed service) and the system proxy setting, then retry `pb doctor`.
    + FullyQualifiedErrorId : NativeCommandError
-- cleanup and proxy start check
C:\hostedtoolcache\windows\Python\3.12.10\x64\python.exe
create rc=0
0
Paperboat daemon service restarted and ready
## W (redo, proxy script restored) 23:53:15Z
-- A: WinINET (HKCU ProxyEnable=1 ProxyServer=127.0.0.1:3128), proxy alive
listening: 3
Paperboat daemon service restarted and ready
Paperboat doctor: healthy
authentication: pass: The Paperboat server accepted the current credential.
proxy log:
10 CONNECT api.pprbt.dev:443
### w > a  23:53:38Z
summary: sent=4 received=4 loss=0.0% connect min/avg/max=264.83/725.86/2105.11ms
609f8b344982
-- B: WinHTTP machine default only (netsh winhttp set proxy), WinINET off
Paperboat daemon service restarted and ready
Paperboat doctor: healthy
authentication: pass: The Paperboat server accepted the current credential.
proxy log:
21 CONNECT api.pprbt.dev:443
-- reset
Paperboat daemon service restarted and ready
Paperboat doctor: healthy
authentication: pass: The Paperboat server accepted the current credential.

## M on dev-69feaf132 (service 'Ethernet'): system proxy honoured, dead proxy named 23:58:13Z
Paperboat daemon service restarted and ready
Paperboat doctor: healthy
authentication: pass: The Paperboat server accepted the current credential.
proxy log:
   8 CONNECT api.pprbt.dev:443
### m > w  23:58:34Z
summary: sent=4 received=4 loss=0.0% connect min/avg/max=30.96/725.03/2601.99ms
exec: runnervmm7ud1
-- proxy stopped (dead system proxy):
Paperboat doctor: unhealthy
authentication: fail: The Paperboat server is unreachable through the configured proxy.
  Recovery: The configured proxy did not accept the connection. Start it, or remove HTTPS_PROXY (PAPERBOAT_HTTPS_PROXY for a managed service) and the system proxy setting, then retry `pb doctor`.
pb: The Paperboat local daemon is running but not ready (degraded): Control plane is unavailable: the configured proxy did not accept the connection. Start the proxy, or remove PAPERBOAT_HTTPS_PROXY (service environment) and the system proxy setting, then restart the daemon. Support reference: suppo
-- removed:
Paperboat daemon service restarted and ready
Paperboat doctor: healthy
authentication: pass: The Paperboat server accepted the current credential.
```

## UDP blocked on M with pf rules in an Apple child anchor (root ruleset untouched, guard stays up): relay both ways with WebSocket fallback named, direct again after the flush
```
## UDP blocked on M (pf rules in the Apple child anchor com.apple/zz-x38, root ruleset untouched) on dev-69feaf132 00:00:09Z
pfctl: Use of -f option, could result in flushing of rules
present in the main ruleset added by the system at startup.
See /etc/pf.conf for further details.
Status: Enabled for 0 days 00:04:57           Debug: Urgent
block drop quick proto udp from any to any port != 53
block drop quick proto udp from any port != 53 to any
### m w
sample 1: path=regional_relay connect=5227.98ms
sample 12: path=regional_relay connect=485.17ms
summary: sent=12 received=12 loss=0.0% connect min/avg/max=479.43/952.01/5227.98ms
exec: runnervmm7ud1
### w m
sample 1: path=regional_relay connect=1185.01ms
sample 12: path=regional_relay connect=480.03ms
summary: sent=12 received=12 loss=0.0% connect min/avg/max=480.03/559.02/1185.01ms
exec: sat12-bq160-517b8142-997f-4a43-adbe-378d7c1c4343-8631469B4FD
-- doctor m>w:
Paperboat doctor: unhealthy
selected_path: warning: The active Paperboat path uses WebSocket fallback.
rules cleared
## anchor flushed 00:01:06Z
sample 1: path=regional_relay connect=736.59ms
sample 8: path=direct connect=32.52ms
summary: sent=8 received=8 loss=0.0% connect min/avg/max=32.23/121.09/736.59ms
sample 1: path=direct connect=32.66ms
sample 8: path=direct connect=31.13ms
summary: sent=8 received=8 loss=0.0% connect min/avg/max=30.93/35.48/59.11ms
-- guard / doctor after:
	state = running
	runs = 1
		state = active
		state = active
Paperboat doctor: healthy
```

## Network change on L, M, W (same method as above) on dev-69feaf132: PASS (execs complete 61/61, path direct again, doctor healthy)
```
## Network change on L, dev-69feaf132: rx38n-a moves bridge x38n -> docker0 (new IPv4) during a 60 s exec a>w 00:01:32Z
baseline: sample 2: path=direct connect=258.82ms transition
exec start 00:01:36
    Packets: Sent = 61, Received = 61, Lost = 0 (0% loss),
exec end 00:02:41
t+20 00:01:56 moving rx38n-a: x38n -> bridge
172.38.8.3 fd3a:38::2 
eth0@if314       UP             172.17.0.19/16 
moved 00:01:57
sample 1: path=direct connect=266.74ms
sample 5: path=direct connect=237.66ms
summary: sent=5 received=5 loss=0.0% connect min/avg/max=237.66/245.90/266.74ms
sample 1: path=direct connect=241.85ms
sample 5: path=direct connect=250.26ms
summary: sent=5 received=5 loss=0.0% connect min/avg/max=238.97/246.23/251.80ms
sample 1: path=direct connect=271.54ms
sample 5: path=direct connect=239.22ms
summary: sent=5 received=5 loss=0.0% connect min/avg/max=238.64/246.93/271.54ms
Paperboat doctor: healthy
peer_paths: pass: Paths: rx38n-w direct.

## Network change on M, dev-69feaf132: network service Ethernet off 15 s then on during a 60 s exec w>m 00:03:02Z
baseline: sample 2: path=regional_relay connect=476.78ms
exec start 00:03:10
EXEC-FINISHED
exec end 00:04:26
cut 00:03:24 192.168.64.11
up 00:03:44 192.168.64.11
sample 1: path=regional_relay connect=711.52ms
sample 20: path=regional_relay connect=694.12ms
summary: sent=20 received=20 loss=0.0% connect min/avg/max=475.17/516.27/711.52ms
sample 1: path=direct connect=91.87ms
sample 20: path=direct connect=31.99ms
summary: sent=20 received=20 loss=0.0% connect min/avg/max=30.48/34.19/91.87ms
Paperboat doctor: healthy
peer_paths: pass: Paths: rx38n-w direct.
   0b5b75495..4dc82e483  HEAD -> main
rc=0
## Network change on W, dev-69feaf132: adapter Ethernet 3 (default route) disabled 15 s then enabled during a 61 s exec m>w 00:05:25Z
baseline: sample 2: path=direct connect=31.64ms
exec start 00:05:32
    Packets: Sent = 61, Received = 61, Lost = 0 (0% loss),
exec end 00:06:37
sample 1: path=direct connect=63.01ms
sample 6: path=direct connect=32.23ms
summary: sent=6 received=6 loss=0.0% connect min/avg/max=31.77/37.57/63.01ms
sample 1: path=direct connect=32.15ms
sample 6: path=direct connect=32.65ms
summary: sent=6 received=6 loss=0.0% connect min/avg/max=31.12/33.11/38.00ms
Paperboat doctor: healthy
authentication: pass: The Paperboat server accepted the current credential.
peer_paths: pass: No connection to another machine is in use.
```
Observation (not a failure): w>m stays regional_relay while m>w is direct (both before and after the cuts); exec works both ways and pb ping states each direction's path.

## Captive portal on L, M and W at once on dev-69feaf132: recovers by itself on all three
Control plane and both edge names answered by a 302 / self-signed HTTPS portal for 150 s (hosts file; L inside the container). Doctor authentication fails on all three while the portal answers (the direct path and exec keep working), and passes within seconds of the removal with no daemon restart. pb doctor does not name a captive portal (the captive_portal check is never set).
```
## Captive portal on L, M and W at once (build dev-69feaf132): control plane and edge names answered by a 302 / self-signed HTTPS portal for 150 s, 00:08:43Z
baseline: a=healthy m=healthy w=healthy
--- 00:09:02 a: unhealthy authentication: fail: The Paperboat server did not accept the authentica | m: unhealthy authentication: fail: The Paperboat server did not accept the authentica | w: unhealthy authentication: fail: The Paperboat server did not accept 
--- 00:09:18 a: unhealthy authentication: fail: The Paperboat server did not accept the authentica | m: unhealthy authentication: fail: The Paperboat server did not accept the authentica | w: unhealthy authentication: fail: The Paperboat server did not accept 
--- 00:09:34 a: unhealthy authentication: fail: The Paperboat server did not accept the authentica | m: unhealthy authentication: fail: The Paperboat server did not accept the authentica | w: unhealthy authentication: fail: The Paperboat server did not accept 
--- 00:09:51 a: unhealthy authentication: fail: The Paperboat server did not accept the authentica | m: unhealthy authentication: fail: The Paperboat server did not accept the authentica | w: unhealthy authentication: fail: The Paperboat server did not accept 
--- 00:10:07 a: unhealthy authentication: fail: The Paperboat server did not accept the authentica | m: unhealthy authentication: fail: The Paperboat server did not accept the authentica | w: unhealthy authentication: fail: The Paperboat server did not accept 
--- 00:10:24 a: unhealthy authentication: fail: The Paperboat server did not accept the authentica | m: unhealthy authentication: fail: The Paperboat server did not accept the authentica | w: unhealthy authentication: fail: The Paperboat server did not accept 
--- 00:10:40 a: unhealthy authentication: fail: The Paperboat server did not accept the authentica | m: unhealthy authentication: fail: The Paperboat server did not accept the authentica | w: unhealthy authentication: fail: The Paperboat server did not accept 
--- 00:10:56 a: unhealthy authentication: fail: The Paperboat server did not accept the authentica | m: unhealthy authentication: fail: The Paperboat server did not accept the authentica | w: unhealthy authentication: fail: The Paperboat server did not accept 
--- 00:11:13 a: unhealthy authentication: fail: The Paperboat server did not accept the authentica | m: unhealthy authentication: fail: The Paperboat server did not accept the authentica | w: unhealthy authentication: fail: The Paperboat server did not accept 
--- 00:11:29 a: unhealthy authentication: fail: The Paperboat server did not accept the authentica | m: healthy authentication: pass: The Paperboat server accepted the current credential | w: healthy authentication: pass: The Paperboat server accepted the curr
portal ON 00:09:01
portal OFF 00:11:31
portal ON 00:09:00
portal OFF 00:11:30
portal ON 00:09:03
portal OFF 00:11:33
--- after 00:12:06: a=healthy m=healthy w=healthy; a>w sample 3: path=direct connect=230.06ms; m>w sample 3: path=direct connect=32.98ms
ff02::2	ip6-allrouters
172.17.0.19	609f8b344982
```

## NAT rebinding on L on dev-69feaf132: single rebind recovers in <25 s, double rebind 15 s apart recovers in ~20 s (twice); ONE earlier double-rebind run stuck on relay a>w for 9+ min until pb service restart (w>a direct); logged in failures.md, not repeated
```
## NAT rebinding on L (dev-69feaf132): the pbvm2 NAT of rx38n-a rewrites every UDP source port to 50000-50500 and the conntrack table is flushed twice during a 
baseline: sample 2: path=direct connect=227.70ms
exec start 00:12:32
    Packets: Sent = 61, Received = 61, Lost = 0 (0% loss),
exec end 00:13:36
t+15 00:12:48 rebinding 1: SNAT udp from 172.17.0.19 to ports 50000-50500, flush conntrack
dst=140.245.11.41 sport=57832 dport=27444 src=140.245.11.41
dst=152.67.0.60 sport=34060 dport=27445 src=152.67.0.60
dst=152.67.0.60 sport=55115 dport=27446 src=152.67.0.60
t+30 00:13:03 new mappings:
t+30 00:13:03 rebinding 2: rule removed, conntrack flushed
sample 1: path=regional_relay connect=287.43ms
sample 8: path=regional_relay connect=261.43ms
summary: sent=8 received=8 loss=0.0% connect min/avg/max=258.13/263.28/287.43ms
sample 1: path=regional_relay connect=263.48ms
sample 8: path=regional_relay connect=259.49ms
summary: sent=8 received=8 loss=0.0% connect min/avg/max=257.56/262.96/284.30ms
sample 1: path=regional_relay connect=259.72ms
sample 8: path=regional_relay connect=265.63ms
summary: sent=8 received=8 loss=0.0% connect min/avg/max=258.72/261.08/265.63ms
Paperboat doctor: healthy
peer_paths: pass: Paths: rx38n-w relay.
0
00:13:55 summary: sent=30 received=30 loss=0.0% connect min/avg/max=257.45/264.02/288.62ms
00:14:05 summary: sent=30 received=30 loss=0.0% connect min/avg/max=258.01/264.42/302.96ms
00:14:14 summary: sent=30 received=30 loss=0.0% connect min/avg/max=256.15/283.11/682.27ms
00:14:24 summary: sent=30 received=30 loss=0.0% connect min/avg/max=257.14/263.41/292.43ms
sample 3: path=regional_relay connect=257.45ms
609f8b344982
00:15:38
summary: sent=40 received=40 loss=0.0% connect min/avg/max=251.96/257.56/283.95ms
sample 2: path=regional_relay connect=252.87ms
00:17:03
summary: sent=40 received=40 loss=0.0% connect min/avg/max=252.55/257.78/283.14ms
sample 2: path=regional_relay connect=286.49ms
sample 2: path=direct connect=278.47ms
00:17:37 a>w sample 2: path=regional_relay connect=272.58m | w>a sample 2: path=direct connect=269.39ms
00:18:03 a>w sample 2: path=regional_relay connect=255.41m | w>a sample 2: path=direct connect=241.39ms
00:18:29 a>w sample 2: path=regional_relay connect=257.29m | w>a sample 2: path=direct connect=244.19ms
00:18:55 a>w sample 2: path=regional_relay connect=257.89m | w>a sample 2: path=direct connect=242.12ms
00:19:20 a>w sample 2: path=regional_relay connect=255.63m | w>a sample 2: path=direct connect=240.44ms
00:19:46 a>w sample 2: path=regional_relay connect=256.53m | w>a sample 2: path=direct connect=241.01ms
00:20:12 a>w sample 2: path=regional_relay connect=272.92m | w>a sample 2: path=direct connect=266.26ms
00:20:39 a>w sample 2: path=regional_relay connect=259.53m | w>a sample 2: path=direct connect=246.21ms
## 00:21:09: a>w still regional_relay 9 min after the rebinding (w>a direct); pb service restart on a:
Paperboat daemon service restarted and ready
00:21:14 sample 1: path=regional_relay connect=1030.62ms sample 3: path=direct connect=243.30ms transition sample 6: path=direct connect=240.03ms 
00:21:19 sample 1: path=direct connect=1727.06ms sample 6: path=direct connect=239.01ms 
00:21:24 sample 1: path=direct connect=243.50ms sample 6: path=direct connect=240.22ms 

baseline 00:21:38 a>w sample 2: path=direct connect=240.05ms | w>a sample 2: path=direct connect=243.50ms
single rebinding at 00:21:45: SNAT rule kept, conntrack flushed once
00:21:45 a>w sample 2: path=regional_relay connect=261.20m | w>a sample 2: path=direct connect=267.64ms
00:22:10 a>w sample 2: path=direct connect=229.58ms | w>a sample 2: path=direct connect=228.05ms
00:22:31 a>w sample 2: path=direct connect=230.00ms | w>a sample 2: path=direct connect=232.47ms
00:22:53 a>w sample 2: path=direct connect=232.43ms | w>a sample 2: path=direct connect=456.45ms
00:23:15 a>w sample 2: path=direct connect=243.73ms | w>a sample 2: path=direct connect=231.27ms
00:23:36 a>w sample 2: path=direct connect=230.48ms | w>a sample 2: path=direct connect=230.86ms
00:23:56 a>w sample 2: path=direct connect=230.25ms | w>a sample 2: path=direct connect=231.19ms
00:24:17 a>w sample 2: path=direct connect=231.06ms | w>a sample 2: path=direct connect=229.99ms
00:24:37 a>w sample 2: path=direct connect=228.50ms | w>a sample 2: path=direct connect=232.70ms
00:24:58 a>w sample 2: path=direct connect=231.67ms | w>a sample 2: path=direct connect=229.96ms
00:25:19 a>w sample 2: path=direct connect=228.56ms | w>a sample 2: path=direct connect=233.18ms
00:25:40 a>w sample 2: path=direct connect=231.45ms | w>a sample 2: path=direct connect=231.33ms
00:26:00 a>w sample 2: path=direct connect=281.35ms | w>a sample 2: path=direct connect=229.91ms
00:26:21 a>w sample 2: path=direct connect=231.66ms | w>a sample 2: path=direct connect=231.65ms
00:26:42 a>w sample 2: path=direct connect=229.27ms | w>a sample 2: path=direct connect=233.02ms
00:27:02 a>w sample 2: path=direct connect=292.64ms | w>a sample 2: path=direct connect=263.28ms
00:27:24 a>w sample 2: path=direct connect=225.80ms | w>a sample 2: path=direct connect=235.22ms
00:27:46 a>w sample 2: path=direct connect=229.54ms | w>a sample 2: path=direct connect=229.98ms
rule removed at 00:28:08
00:29:13 a>w sample 2: path=direct connect=236.64ms | w>a sample 2: path=direct connect=237.76ms
00:29:20 a>w sample 2: path=direct connect=237.92ms | w>a sample 2: path=direct connect=242.85ms
00:29:26 a>w sample 2: path=direct connect=239.02ms | w>a sample 2: path=direct connect=237.60ms

baseline 00:29:38 a>w sample 2: path=direct connect=239.39ms | w>a sample 2: path=direct connect=238.40ms
00:29:44 a>w sample 2: path=direct connect=238.20ms | w>a sample 2: path=direct connect=239.26ms
t+15 00:30:00 rebinding 1: SNAT udp from 172.17.0.19 to ports 50000-50500, flush conntrack
dst=10.1.0.10 sport=41642 dport=58274 [UNREPLIED]
dst=140.245.11.41 sport=60305 dport=27445 src=140.245.11.41
dst=140.245.11.41 sport=41642 dport=27445 src=140.245.11.41
00:30:00 a>w sample 2: path=direct connect=243.79ms | w>a sample 2: path=direct connect=236.76ms
t+30 00:30:15 new mappings:
t+30 00:30:15 rebinding 2: rule removed, conntrack flushed
00:30:20 a>w sample 2: path=regional_relay connect=259.04m | w>a sample 2: path=direct connect=229.87ms
00:30:38 a>w sample 2: path=direct connect=237.68ms | w>a sample 2: path=direct connect=231.67ms
00:30:54 a>w sample 2: path=direct connect=239.55ms | w>a sample 2: path=direct connect=234.05ms
00:31:10 a>w sample 2: path=direct connect=237.83ms | w>a sample 2: path=direct connect=226.66ms
00:31:28 a>w sample 2: path=direct connect=239.09ms | w>a sample 2: path=direct connect=227.44ms
00:31:44 a>w sample 2: path=direct connect=239.09ms | w>a sample 2: path=direct connect=227.87ms
00:32:01 a>w sample 2: path=direct connect=226.28ms | w>a sample 2: path=direct connect=236.34ms
00:32:16 a>w sample 2: path=direct connect=231.75ms | w>a sample 2: path=direct connect=438.16ms
00:32:34 a>w sample 2: path=direct connect=228.20ms | w>a sample 2: path=direct connect=238.41ms
00:32:51 a>w sample 2: path=direct connect=231.09ms | w>a sample 2: path=direct connect=238.52ms
00:33:07 a>w sample 2: path=direct connect=448.66ms | w>a sample 2: path=direct connect=238.85ms
00:33:23 a>w sample 2: path=direct connect=0.05ms | w>a sample 2: path=direct connect=234.89ms
00:33:38 a>w sample 2: path=direct connect=230.97ms | w>a sample 2: path=direct connect=236.50ms
00:33:54 a>w sample 2: path=direct connect=242.34ms | w>a sample 2: path=direct connect=224.70ms
```

## Full-tunnel VPN on L on dev-69feaf132: WireGuard default route through wg0, endpoint kept on eth0: PASS (a<>w direct, exec both ways, doctor healthy, 61 ... 21-packet exec across it)
```
## Full-tunnel VPN on L (dev-69feaf132): WireGuard wg0 in rx38n-a, default route through wg0 to pbvm2 (NAT gateway), endpoint kept
interface: wgx38
  public key: 4ImR865skOoM+vSrp5cdMdWX/SBZbu5D4CxrVN7TlD4=
  private key: (hidden)
default dev wg0 scope link 
10.38.77.0/24 dev wg0 proto kernel scope link src 10.38.77.2 
172.17.0.0/16 dev eth0 proto kernel scope link src 172.17.0.19 
172.17.0.1 dev eth0 scope link 
interface: wg0
  public key: uSTSzsYUYDR6SXKlh6S79DiVjIV6Fg7kyLb82pzITVQ=
  private key: (hidden)
  listening port: 32921
-- through the tunnel: public address seen by the API: 49.36.126.248, pbvm2's own: 49.36.126.248
### a > w  00:31:56Z
sample 4: path=direct connect=239.48ms
exec: runnervmm7ud1
### w > a  00:32:17Z
sample 4: path=direct connect=451.57ms
exec: 
### a > m  00:32:30Z
sample 4: path=regional_relay connect=297.23ms
exec: sat12-bq160-517b8142-997f-4a43-adbe-378d7c1c4343-8631469B4FD8.local
### m > a  00:32:38Z
sample 4: path=regional_relay connect=283.33ms
exec: 609f8b344982
### a > v6  00:32:50Z
sample 4: path=regional_relay connect=68.04ms
exec: 343406103e99
### v6 > a  00:32:56Z
sample 4: path=regional_relay connect=62.73ms
exec: 609f8b344982
-- longer: a>w 40 samples
sample 1: path=direct connect=257.06ms
sample 40: path=direct connect=230.41ms
summary: sent=40 received=40 loss=0.0% connect min/avg/max=224.87/242.96/446.76ms
Paperboat doctor: healthy
authentication: pass: The Paperboat server accepted the current credential.
peer_paths: pass: Paths: rx38n-mb relay, rx38n-w direct.
    Packets: Sent = 21, Received = 21, Lost = 0 (0% loss),
default via 172.17.0.1 dev eth0 
172.17.0.0/16 dev eth0 proto kernel scope link src 172.17.0.19 
172.17.0.1 dev eth0 scope link 
down
Device "wgx38" does not exist.
after teardown: Paperboat doctor: healthy; a>w sample 3: path=direct connect=238.85ms
```

## pb connect on dev-69feaf132 from the IPv6-only machine (pty): shell on the Mac reached (hostname printed), shell on Windows reached (C:\Users\runneradmin> prompt)
```
## pb connect on dev-69feaf132 (pty): IPv6-only v6 > mac, IPv6-only v6 > windows
CONNECT-OK-sat12-bq160-517b8142-997f-4a43-adbe-378d7c1c4343-8631469B4FD8.local
bash: line 1: printf: `C': invalid format character
]2;]9;9;C:\Users\runneradmin[?25lCONNECT-OK-exit[7;1HC:\Users\runneradmin>[?25h^D
```

## Final pair table on dev-69feaf132 (12 directed pairs including the IPv6-only machine)
```
## All pairs on dev-69feaf132 (final): ping x4 (samples 1 and 4, summary) + exec hostname
pb version a: dev-69feaf132
pb version v6: dev-69feaf132
pb version m: dev-69feaf132
pb version w: dev-69feaf132
### a > m  00:34:11Z
sample 1: path=regional_relay connect=1913.61ms
sample 4: path=regional_relay connect=299.75ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=295.07/701.37/1913.61ms
exec: sat12-bq160-517b8142-997f-4a43-adbe-378d7c1c4343-8631469B4FD8.local
### m > a  00:34:23Z
sample 1: path=regional_relay connect=848.23ms
sample 4: path=regional_relay connect=286.75ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=285.82/426.88/848.23ms
exec: 609f8b344982
### a > w  00:34:35Z
sample 1: path=direct connect=1743.88ms
sample 4: path=direct connect=239.70ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=237.17/615.49/1743.88ms
exec: runnervmm7ud1
### w > a  00:34:43Z
sample 1: path=direct connect=673.11ms
sample 4: path=direct connect=224.43ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=224.43/339.37/673.11ms
exec: 609f8b344982
### m > w  00:34:54Z
sample 1: path=regional_relay connect=1207.95ms
sample 4: path=regional_relay connect=479.88ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=478.42/661.49/1207.95ms
exec: runnervmm7ud1
### w > m  00:35:08Z
sample 1: path=regional_relay connect=1476.24ms
sample 4: path=regional_relay connect=495.56ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=491.80/739.37/1476.24ms
exec: sat12-bq160-517b8142-997f-4a43-adbe-378d7c1c4343-8631469B4FD8.local
### v6 > a  00:35:23Z
sample 1: path=regional_relay connect=180.28ms
sample 4: path=regional_relay connect=65.23ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=63.64/93.21/180.28ms
exec: 609f8b344982
### a > v6  00:35:28Z
sample 1: path=regional_relay connect=180.73ms
sample 4: path=regional_relay connect=57.19ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=57.19/90.11/180.73ms
exec: 343406103e99
### v6 > m  00:35:33Z
sample 1: path=regional_relay connect=874.95ms
sample 4: path=regional_relay connect=303.33ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=292.63/441.74/874.95ms
exec: sat12-bq160-517b8142-997f-4a43-adbe-378d7c1c4343-8631469B4FD8.local
### m > v6  00:35:41Z
sample 1: path=regional_relay connect=846.03ms
sample 4: path=regional_relay connect=282.78ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=281.27/423.06/846.03ms
exec: 343406103e99
### v6 > w  00:35:52Z
sample 1: path=regional_relay connect=553.10ms
sample 4: path=regional_relay connect=263.34ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=262.06/336.16/553.10ms
exec: runnervmm7ud1
### w > v6  00:36:00Z
sample 1: path=regional_relay connect=790.69ms
sample 4: path=regional_relay connect=264.12ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=264.12/396.14/790.69ms
exec: 343406103e99
```
