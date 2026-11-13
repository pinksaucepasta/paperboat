# X-38 part 3 by hand on my own Linux machines (containers on hp), UTC 2026-11-13 12:30 - 13:40 (IST 18:00 - 19:10)

Build on every machine: `pb version` dev-d77a40713 (= origin/main d77a40713 when the run started). Machines (all Ubuntu 24.04 + systemd user service, `pb install` of that build, paired with `pb pair` into the lane test account):
- x38-l (machine alias x38-l-2): **IPv6 only**. Docker bridge `x38v6` (fd38:38::/64) with NAT64 (tayga, prefix 64:ff9b:1::/96) and DNS64 (unbound) on hp; the container's IPv4 address, IPv4 routes removed (`ip -4 addr` shows only 127.0.0.1) and resolv.conf points to the DNS64 resolver.
- x38-p, x38-f, x38-g: dual-stack (172.38.0.0/24 behind masquerade + fd38:38::/64 ULA) on the same bridge.
- hp itself runs Tailscale (BackendState Running, no exit node, default route via wlo1); the pbmac VM has no Tailscale CLI/app and its default route is en0; neither is a full-tunnel VPN. The full-tunnel parts are the WireGuard default-route passes of part 2 (Linux and Windows).

## IPv6 only: FAIL (two machines can accept, not start, connections)
- Reaching the control plane through NAT64/DNS64 works: `pb pair` OK, `pb doctor` healthy (all checks pass incl. udp_ipv4/udp_ipv6, local_state, machine_credentials).
- Dual-stack peer to the v6-only machine: `pb ping` 4/4 `path=direct`, `pb exec ... hostname` returns (x38-p>x38-l, x38-f>x38-l). Packet capture on the bridge shows the direct path is IPv6 UDP between fd38:38::2 and fd38:38::3/::4.
- v6-only machine to the dual-stack peer (x38-l>x38-p, x38-l>x38-f): `pb ping` `timeout` x4, 100% loss, `pb exec` "Paperboat could not reach the target machine"; `pb doctor <peer>` selected_path fail "No path ... the last connection attempt failed".
- Cause (diagnostic bundle of x38-l): daemon event operation=preview_attachment cause=network_unreachable errno 101, error chain TransportDialError:HTTPCarrierFailure. The edge carrier/relay endpoints the server hands out are IPv4 literals (server/internal/runtimeattachment/service.go:275 passes `carrier_endpoint_host` through unchanged; operator data, accepts DNS names), and an IPv4 literal does not go through DNS64, so a host without an IPv4 route (NAT64 without CLAT) cannot dial the carrier or the UDP relay.
- Proof: giving x38-l an IPv4 address and routing only the two carrier/relay literals (/32 routes via the bridge) over IPv4, everything else still IPv6, makes `pb ping` x38-l>x38-p `path=direct` 4/4 at once; removing them brings the failure back.
- Fix direction (not done here): publish DNS names with A and AAAA records for the hosted relay/edge nodes and set carrier_endpoint_host / endpoint_host to the names (the carrier certificate needs the name as SAN: server/internal/controlplane/edge.go:679). Tailscale's DERP map uses host names for the same reason. Logged in failures.md.

## Dual-stack: PASS
x38-p, x38-f, x38-g (IPv4 + IPv6 on one interface): `pb ping` `path=direct` both ways, `pb exec` both ways, `pb doctor` healthy (see the firewall tables below, same machines). x38-p>x38-l uses the IPv6 path (capture above), x38-g>x38-f the IPv4 or IPv6 one; `pb ping` states the path, not the family (no command prints v4/v6, noted).

## Captive portal: PASS (recovers by itself), diagnosis not named
Machine x38-p (dual-stack), peer x38-l. Portal built in the container's own network namespace (nft table x38portal, removed afterwards): every new outbound TCP 80 is redirected to a local server answering `302 Found` to a login page, every TCP 443 to a TLS server with a self-signed certificate answering a "Sign in to the guest Wi-Fi" HTML page, existing flows get a reset, all other UDP is dropped (DNS passes). Self-test inside the portal: `curl http://...` -> `HTTP/1.0 302 Found`, `curl -k https://...` -> the HTML page, `curl https://api...` without -k -> certificate verify failure. (A first run of this part had a portal server that crashed on start: it was a plain blackout; kept as portal-blackout, result the same, not used.)
```
--- 13:15:14 baseline
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-d77a40713
sample 3: path=direct connect=5.37ms
sample 4: path=direct connect=5.10ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=5.10/87.83/332.70ms
Paperboat doctor: healthy
authentication: pass: The Paperboat server accepted the current credential.
--- 13:15:20 portal ON
HTTP/1.0 302 Found
Server: BaseHTTP/0.6 Python/3.12.3
Date: Fri, 13 Nov 2026 13:15:23 GMT
<html><body>Sign in to the guest Wi-Fi</body></html>--- 13:15:43 portal +20s
Paperboat doctor: unhealthy
authentication: fail: The Paperboat server did not accept the authenticated health check.
  Recovery: Check network access and sign in again if needed, then run pb doctor again.
clock: warning: The Paperboat server could not be reached to read its clock.
  Recovery: Check the connection to Paperboat and run `pb doctor` again.
sample 4: timeout
summary: sent=4 received=0 loss=100.0%
  Path: none  Consumers: 0  Transfer: degraded  Preview: degraded  SSH: unavailable
  Health: warning: Machine runtime is offline  Recovery: Check the Paperboat service on the machine
  Path: none  Consumers: 0  Transfer: degraded  Preview: degraded  SSH: unavailable
  Health: warning: Machine runtime is offline  Recovery: Check the Paperboat service on the machine
--- 13:17:25 portal +60s
Paperboat doctor: unhealthy
authentication: fail: The Paperboat server did not accept the authenticated health check.
  Recovery: Check network access and sign in again if needed, then run pb doctor again.
clock: warning: The Paperboat server could not be reached to read its clock.
  Recovery: Check the connection to Paperboat and run `pb doctor` again.
daemon: warning: The local daemon is degraded. Control plane is unavailable.
  Recovery: Check network access and Paperboat authentication.
daemon:control_plane_unavailable: fail: Control plane is unavailable
  Recovery: Check network access and Paperboat authentication
sample 4: timeout
summary: sent=4 received=0 loss=100.0%
Health: error: Control plane is unavailable  Recovery: Check network access and Paperboat authentication
  Path: none  Consumers: 0  Transfer: degraded  Preview: degraded  SSH: unavailable
  Health: warning: Machine runtime is offline  Recovery: Check the Paperboat service on the machine
  Path: none  Consumers: 0  Transfer: degraded  Preview: degraded  SSH: unavailable
--- 13:19:08 portal +60s
Paperboat doctor: unhealthy
authentication: fail: The Paperboat server did not accept the authenticated health check.
  Recovery: Check network access and sign in again if needed, then run pb doctor again.
clock: warning: The Paperboat server could not be reached to read its clock.
  Recovery: Check the connection to Paperboat and run `pb doctor` again.
daemon: warning: The local daemon is degraded. Control plane is unavailable.
  Recovery: Check network access and Paperboat authentication.
daemon:control_plane_unavailable: fail: Control plane is unavailable
  Recovery: Check network access and Paperboat authentication
peer_paths: fail: No path to x38-l-2: the last connection attempt failed.
  Recovery: Check that x38-l-2 is online and that UDP and HTTPS to the relays are allowed here and there, then run pb doctor x38-l-2. If it persists, run pb bugreport.
sample 4: timeout
summary: sent=4 received=0 loss=100.0%
Health: error: Control plane is unavailable  Recovery: Check network access and Paperboat authentication
  Path: none  Consumers: 0  Transfer: degraded  Preview: degraded  SSH: unavailable
  Health: warning: Machine runtime is offline  Recovery: Check the Paperboat service on the machine
  Path: none  Consumers: 0  Transfer: degraded  Preview: degraded  SSH: unavailable
--- 13:19:50 portal OFF
221
--- 13:20:06 recovery +15s
Paperboat doctor: unhealthy
sample 4: path=direct connect=10.14ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=7.22/671.02/2646.81ms
--- 13:20:56 recovery +45s
Paperboat doctor: healthy
sample 4: path=direct connect=4.95ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=4.95/39.06/132.81ms
--- 13:22:44 recovery +90s
Paperboat doctor: healthy
sample 4: path=direct connect=8.65ms
summary: sent=4 received=4 loss=0.0% connect min/avg/max=4.74/82.28/303.92ms
--- 13:22:48 done
rc=0
```
Result: during the portal (4.5 min) `pb doctor` is unhealthy (authentication fail, control plane unavailable, no path) and nothing is wrong afterwards: the portal was removed at 13:19:50, `pb ping` is direct 4/4 at +15 s, `pb doctor` healthy at +45 s, with no restart of the daemon. Gap (not the row text): doctor does not say "captive portal"; its recovery text says "sign in again if needed" and the `captive_portal` check always prints "could not be observed" (typed at cli/internal/localapi/types.go:517, no producer). Logged in failures.md.

## Linux host firewalls in their default state: PASS (no prompt exists on Linux)
Machines x38-f (firewall on) and x38-g, both directions per round: `pb ping` (4 samples), `pb exec -- hostname`, plus `pb doctor` on x38-f.
- ufw (`ufw default deny incoming`, `default allow outgoing`, enabled; 63 ufw nft chains), one round (a 3-round rerun was cut short when hp went offline): every ping `path=direct`, exec both ways, doctor healthy.
- firewalld (package default, zone public; started with its nft table `inet firewalld`; its D-Bus API does not answer inside a container, the loaded nft rules are the default): 3 rounds, every ping `path=direct` 4/4, exec both ways OK, doctor healthy. (A first attempt right after starting it, under a host load of 300, timed out once; see below.)
- nftables stateful default-drop input (`policy drop`, established/related, lo, icmp accepted): 5 rounds clean, doctor healthy. One earlier round (3 rounds) had one `pb exec` x38-f>x38-g fail ("could not reach the target machine") while the ping beside it was direct 4/4; not repeated in the next 5 rounds.
firewalld/nftables round output, trimmed:
```
--- 13:35:37 firewalld default zone public (daemon started, nft table inet firewalld loaded)
load average: 60.09, 196.23, 145.13
table inet firewalld
table inet firewalld {
	chain filter_IN_public {
		jump filter_IN_public_pre
		jump filter_IN_public_log
		jump filter_IN_public_deny
		jump filter_IN_public_allow
		jump filter_IN_public_post
		meta l4proto { icmp, ipv6-icmp } accept
	}
}
round 1
summary: sent=4 received=4 loss=0.0% connect min/avg/max=5.39/10.37/24.24ms
cf72929fc5b8
summary: sent=4 received=4 loss=0.0% connect min/avg/max=5.14/14.76/41.56ms
ff0f773cabe4
round 2
summary: sent=4 received=4 loss=0.0% connect min/avg/max=4.15/5.32/6.64ms
cf72929fc5b8
summary: sent=4 received=4 loss=0.0% connect min/avg/max=4.77/24.32/56.41ms
ff0f773cabe4
round 3
summary: sent=4 received=4 loss=0.0% connect min/avg/max=4.44/6.14/7.82ms
cf72929fc5b8
summary: sent=4 received=4 loss=0.0% connect min/avg/max=3.97/4.71/6.05ms
ff0f773cabe4
Paperboat doctor: healthy
1
--- 13:36:08 nftables stateful default-drop input
load average: 38.52, 177.98, 140.68
table inet x38fw {
	chain input {
		type filter hook input priority filter; policy drop;
		ct state established,related accept
		iif "lo" accept
		meta l4proto { icmp, ipv6-icmp } accept
	}
	chain forward {
round 1
summary: sent=4 received=4 loss=0.0% connect min/avg/max=3.90/5.41/7.06ms
cf72929fc5b8
summary: sent=4 received=4 loss=0.0% connect min/avg/max=4.46/5.04/5.64ms
ff0f773cabe4
round 2
summary: sent=4 received=4 loss=0.0% connect min/avg/max=4.44/4.71/5.27ms
cf72929fc5b8
summary: sent=4 received=4 loss=0.0% connect min/avg/max=3.67/4.91/7.19ms
ff0f773cabe4
round 3
summary: sent=4 received=4 loss=0.0% connect min/avg/max=3.87/4.96/7.21ms
pb: Paperboat could not reach the target machine. It may be 
summary: sent=4 received=4 loss=0.0% connect min/avg/max=5.44/6.22/7.29ms
ff0f773cabe4
Paperboat doctor: healthy
0
--- 13:36:38 done
load average: 24.44, 161.23, 136.29
rc=0
--- 13:36:52 nftables stateful default-drop input
load average: 20.32, 153.62, 134.20
table inet x38fw {
	chain input {
		type filter hook input priority filter; policy drop;
		ct state established,related accept
		iif "lo" accept
		meta l4proto { icmp, ipv6-icmp } accept
	}
	chain forward {
round 1
summary: sent=4 received=4 loss=0.0% connect min/avg/max=4.04/5.77/6.82ms
cf72929fc5b8
summary: sent=4 received=4 loss=0.0% connect min/avg/max=3.99/6.21/10.13ms
ff0f773cabe4
round 2
summary: sent=4 received=4 loss=0.0% connect min/avg/max=4.10/11.95/24.65ms
cf72929fc5b8
summary: sent=4 received=4 loss=0.0% connect min/avg/max=6.14/6.94/8.15ms
ff0f773cabe4
round 3
summary: sent=4 received=4 loss=0.0% connect min/avg/max=3.80/5.00/6.26ms
cf72929fc5b8
summary: sent=4 received=4 loss=0.0% connect min/avg/max=4.28/5.11/5.74ms
ff0f773cabe4
round 4
summary: sent=4 received=4 loss=0.0% connect min/avg/max=3.81/4.06/4.66ms
cf72929fc5b8
summary: sent=4 received=4 loss=0.0% connect min/avg/max=3.86/4.93/6.37ms
ff0f773cabe4
round 5
summary: sent=4 received=4 loss=0.0% connect min/avg/max=4.29/6.52/10.28ms
cf72929fc5b8
summary: sent=4 received=4 loss=0.0% connect min/avg/max=4.40/5.33/6.39ms
ff0f773cabe4
Paperboat doctor: healthy
0
--- 13:37:27 done
load average: 12.70, 137.02, 129.37
rc=0
```
Host load: hp reached load 330 from other lanes during the first firewalld attempt (pings 100% loss, 1-5 s) and later became unreachable; the numbers above are from runs at load 12-60.

## Not run (named gaps)
- Wi-Fi switch (a machine moving between two networks with a new address, gateway and NAT while an exec runs) and the daemon-level proxy check (shell has a proxy, service has none; service has it): scripts were ready, but hp went unreachable at 19:05 IST (Tailscale offline, ssh timeout) after load 330 from other lanes, and pbvm2 stayed at load 160-230 for the next hour (container create/removal and `pb machine add` took minutes or failed); a result taken there would not be a product result.
- Full-tunnel VPN on M and W, and every part above on M and W: unchanged from part 2 (WireGuard full tunnel passes on Linux and Windows only).
- IPv6-only needs the hosted endpoints to be names (see the failure above) before a rerun can pass.
