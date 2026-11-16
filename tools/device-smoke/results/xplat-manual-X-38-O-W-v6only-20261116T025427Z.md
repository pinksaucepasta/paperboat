# X-38 evidence: Windows machine, dual-stack then IPv6-only (manual, 2026-11-16 08:24 IST)

Build `dev-432169341` (windows/amd64 sha256 `7b3ee180a48a54be…`) on **rx38o-w**; Linux peer **rx38o-a** (pbvm2 container, same build).
Prod server and relays. Manual run, scripted by hand (not in smoke): the setup below is reproducible but no smoke id covers it yet.

## Bed

- Windows: a **Windows Sandbox** (ephemeral Hyper-V guest, Win11 26100) on the Windows host victus. Its own network is IPv4 NAT only (no IPv6).
  Driven with `wsb` from a scheduled task in the host's interactive session (ssh sessions see no sandbox) and `wsb exec -r System`.
- IPv6 for the guest: **WireGuard for Windows 0.5.3** (official MSI) to a WireGuard endpoint on pbvm2 (`x38ow`, udp 51888, inner `fd38:0:0:1::/64`, `10.38.0.0/24`).
  AllowedIPs `2000::/3, 64:ff9b::/96, fd38:0:0:1::/64` (not `::/0`: WireGuard then blocks all IPv4 on the guest).
  pbvm2 masquerades `fd38:0:0:1::/64` out of eth0 (global IPv6) and routes `64:ff9b::/96` into tayga (NAT64); a second unbound (`x38o-unbound-w3`, `/etc/unbound/x38o-w.conf`) serves DNS64 on `fd38:0:0:1::1`.
- pb ran as a local administrator (`pbw`, elevated scheduled task): `pb install` as SYSTEM fails (see Findings).

## Phase A: dual-stack (IPv4 NAT default route + IPv6 through the tunnel)

Guest, before pb:
```
   Primary Dns Suffix  . . . . . . . : 
Unknown adapter x38dual:
   Connection-specific DNS Suffix  . : 
   IPv6 Address. . . . . . . . . . . : fd38:0:0:1::2(Preferred) 
   IPv4 Address. . . . . . . . . . . : 10.38.0.2(Preferred) 
   Default Gateway . . . . . . . . . : 
Ethernet adapter Ethernet:
   Connection-specific DNS Suffix  . : mshome.net
   Description . . . . . . . . . . . : Microsoft Hyper-V Network Adapter
   Link-local IPv6 Address . . . . . : fe80::3682:414d:3557:cee5%205(Preferred) 
   IPv4 Address. . . . . . . . . . . : 172.27.233.48(Preferred) 
   Default Gateway . . . . . . . . . : 172.27.224.1
   DNS Servers . . . . . . . . . . . : 172.27.224.1
          0.0.0.0          0.0.0.0     172.27.224.1    172.27.233.48     15
        10.38.0.1  255.255.255.255         On-link         10.38.0.2      5
        10.38.0.2  255.255.255.255         On-link         10.38.0.2    261
        224.0.0.0        240.0.0.0         On-link         127.0.0.1    331
        224.0.0.0        240.0.0.0         On-link     172.27.233.48    271
===========================================================================
Interface List
209...........................WireGuard Tunnel
205...00 15 5d b4 dd bf ......Microsoft Hyper-V Network Adapter
200...........................Software Loopback Interface 1
===========================================================================
IPv6 Route Table
===========================================================================
Active Routes:
 If Metric Network Destination      Gateway
200    331 ::1/128                  On-link
209      5 64:ff9b::/96             On-link
209      5 2000::/3                 On-link
209      5 fd38:0:0:1::/64          On-link
209    261 fd38:0:0:1::2/128        On-link
205    271 fe80::/64                On-link
205    271 fe80::3682:414d:3557:cee5/128
                                    On-link
200    331 ff00::/8                 On-link
205    271 ff00::/8                 On-link
===========================================================================
Persistent Routes:
  None
```
`curl -4 https://api.pprbt.dev` -> 404 (server answers), `curl -g https://[2404:6800:4000:1017::5e]/` (public IPv6) -> 404, `curl -6` via 64:ff9b:: -> 404.

`pb install` ok, `pb pair --name rx38o-w` ok, `pb doctor`: healthy (authentication, daemon, machine_credentials, udp_ipv4, udp_ipv6 pass).

rx38o-w -> rx38o-a:
```
== pb ping rx38o-a
sample 1: path=regional_relay connect=775.48ms
sample 2: path=regional_relay connect=0.00ms
sample 3: path=regional_relay connect=0.00ms
summary: sent=3 received=3 loss=0.0% connect min/avg/max=0.00/258.49/775.48ms
== pb exec rx38o-a
```
rx38o-a -> rx38o-w (`pb ping rx38o-w --count 3`; `pb exec rx38o-w -- cmd.exe /c echo ok`):
```
sample 1: path=direct connect=1061.77ms
sample 2: path=direct connect=1.98ms
sample 3: path=direct connect=0.09ms
summary: sent=3 received=3 loss=0.0% connect min/avg/max=0.09/354.61/1061.77ms
ok
```

## Phase B: IPv6 only

Change: tunnel replaced by the v6-only config (`Address = fd38:0:0:1::2/128`, `DNS = fd38:0:0:1::1`), `route delete 0.0.0.0`, a /32 route to the tunnel endpoint 192.168.31.146 only.
Guest after:
```
   Primary Dns Suffix  . . . . . . . : 
Unknown adapter x38v6:
   Connection-specific DNS Suffix  . : 
   IPv6 Address. . . . . . . . . . . : fd38:0:0:1::2(Preferred) 
   Autoconfiguration IPv4 Address. . : 169.254.33.71(Preferred) 
   Default Gateway . . . . . . . . . : 
   DNS Servers . . . . . . . . . . . : fd38:0:0:1::1
Ethernet adapter Ethernet:
   Connection-specific DNS Suffix  . : mshome.net
   Description . . . . . . . . . . . : Microsoft Hyper-V Network Adapter
   Link-local IPv6 Address . . . . . : fe80::3682:414d:3557:cee5%205(Preferred) 
   IPv4 Address. . . . . . . . . . . : 172.27.233.48(Preferred) 
   Default Gateway . . . . . . . . . : 
   DNS Servers . . . . . . . . . . . : 172.27.224.1
   192.168.31.146  255.255.255.255     172.27.224.1    172.27.233.48     16
        224.0.0.0        240.0.0.0         On-link         127.0.0.1    331
        224.0.0.0        240.0.0.0         On-link     172.27.233.48    271
200    331 ::1/128                  On-link
209      5 64:ff9b::/96             On-link
209      5 2000::/3                 On-link
209      5 fd38:0:0:1::/64          On-link
209    261 fd38:0:0:1::2/128        On-link
205    271 fe80::/64                On-link
205    271 fe80::3682:414d:3557:cee5/128
```
Proof that IPv4 is unusable and IPv6 works (guest, same moment):
```
route print -4: no 0.0.0.0/0 route (only 192.168.31.146/32 via 172.27.224.1)
ping 8.8.8.8: PING: transmit failed. General failure.
curl -4 https://api.pprbt.dev  -> code=000 (no path)
curl -4 https://1.1.1.1/       -> code=000
nslookup api.pprbt.dev (server fd38:0:0:1::1, DNS64) -> 64:ff9b::9db4:4a58
curl -6 https://api.pprbt.dev  -> code=404 remote=64:ff9b::9db4:4a58   (reached through NAT64)
```
pbvm2 conntrack while rx38o-w pinged: its flows to the prod relays (152.67.0.60, 140.245.11.41, udp 27444-27446) leave as NAT64 source 192.168.254.57 (tayga pool); `wg show x38ow` transfer 9.2 MB in / 12.4 MB out.

rx38o-w -> rx38o-a (after 30 s settle):
```
== pb doctor
Paperboat doctor: healthy
Correlation: support_dea9e39a-426d-4c3f-8c10-d3c9b0befc6c
authentication: pass: The Paperboat server accepted the current credential.
clock: pass: This machine's clock matches the Paperboat server's.
daemon: pass: The local daemon is ready.
local_names: pass: Local browser names resolve to this computer.
local_state: pass: Local Paperboat state and identity are valid.
machine_credentials: pass: The Paperboat server accepts this machine's credentials.
machine_guard_version: pass: The machine guard runs this pb's version.
peer_paths: pass: No connection to another machine is in use.
udp_ipv4: pass: IPv4 UDP sockets are available.
udp_ipv6: pass: IPv6 UDP sockets are available.
== pb ping rx38o-a
sample 1: path=regional_relay connect=335.44ms
sample 2: path=regional_relay connect=0.57ms
sample 3: path=regional_relay connect=0.31ms
summary: sent=3 received=3 loss=0.0% connect min/avg/max=0.31/112.11/335.44ms
== pb exec rx38o-a
ok
```
rx38o-a -> rx38o-w:
```
sample 1: path=regional_relay connect=216.24ms
sample 2: path=regional_relay connect=0.26ms
sample 3: path=regional_relay connect=0.16ms
summary: sent=3 received=3 loss=0.0% connect min/avg/max=0.16/72.22/216.24ms
ok
```

## Results

| Item | Dual-stack | IPv6-only |
|---|---|---|
| `pb doctor` rx38o-w | healthy | healthy |
| `pb ping` W -> A | pass, path=regional_relay | pass, path=regional_relay |
| `pb exec` W -> A (`echo ok`) | pass | pass |
| `pb ping` A -> W | pass, path=direct | pass, path=regional_relay |
| `pb exec` A -> W (`cmd.exe /c echo ok`) | pass | pass |
| `pb connect` | not run: needs an interactive terminal (the guest has none) | not run |

## Findings and cautions

- Not a product bug in the build under test: the pbvm2 NAT64 bed (tayga + unbound) did not forward (FORWARD policy DROP in iptables/ip6tables); I added ACCEPT rules for `x38onat64`, `x38ow` (comment `x38o-w`).
- `pb install` run as SYSTEM (what `wsb exec -r System` gives) fails: the local daemon service cannot start, Windows error 1212 "The format of the specified domain name is invalid" (`PaperboatLocalDaemon-…-startup-error.log`); a normal elevated local admin installs fine. Windows is a per-user product; not logged as a bug.
- `pb pair` from a non-interactive elevated task printed "Still working on the administrator step" for over 2 minutes and my job timeout cut it before the final JSON; the machine was nevertheless enrolled and healthy afterwards (services running). Not reproduced on a real session; worth a look when pair is run from a scheduled task.
- WireGuard with `AllowedIPs = ::/0` alone made every IPv4 packet on the guest fail ("General failure"), so the tunnel uses `2000::/3` plus the NAT64 prefix.
- The paths in the table are from one run each; the A -> W path moved from direct to relay once IPv4 was removed, as expected.
