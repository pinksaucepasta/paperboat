# RERUN-WINCUT: Windows checks rerun with a cut proven by capture, 2026-11-17 IST

Why: fix-winpathlabel-20261117.md found that a Windows Firewall block only applies to new flows, so a pb.exe already
talking to the peer kept passing. Its fix makes winhost.NET_CUT / UDP_BLOCK stop the pb.exe that own the covered
sockets. Checks whose PASS could be false (verdict A there) are rerun below, each with a capture that proves the cut.

Machines: winpool-2 (GitHub-hosted Windows Server 2025 Datacenter, machine winpool-rwc-2, public 20.120.164.96) and
rwc-l (throwaway Linux container on pbvm2, ~/laneh mkct.sh). Both on `pb version` dev-b112b2a4a, the build the
04:00 IST build hold allows against prod (server .48 + relay .29); paired in the production test account.
Harness: main c3cb3305d + this commit.

## How the cut is made now

- Survival checks (terminal N4/N5, rebind) and the dead-path check must not restart pb.exe, so NET_CUT is not used:
  `SMOKE_PEER_CUT="container_cut.sh pbvm2 rwc-l"` cuts the PEER instead (winhost.peer_cut).
  - `add all` / `add server IP`: iptables + ip6tables DROP inside the container (it runs --privileged). The proof is a
    tcpdump in the container's network namespace on eth0, started after the rules are in place and stopped before they
    are lifted. "sent" counts IP/IPv6 packets with the container's own source addresses (ARP is not netfilter's).
  - `add silent IP`: DROP in pbvm2's DOCKER-USER (forward) chain. The container's sends still succeed, the way they
    do on a real dead path. A drop in the container's own OUTPUT makes sendmsg fail with EPERM, and pb takes that as
    the path being down at once: the first direct_path_cut run measured a 0.8 s gap that way, so it is not counted.
    The proof is a host tcpdump (`sent` = packets of the flow that reached the wire) plus the rule counters.
- PR-02 keeps UDP_BLOCK on the device (the check restarts the daemon anyway). Fixed in c3cb3305d: the rule now takes the
  ports udp_blocked computes, so 443 is really allowed (before, it always blocked 1-52,54-65535).

## Results

| check | rows (W) | result | cut proven by |
|---|---|---|---|
| terminal_matrix N4 network drop (45 s) | F3-15, F3-16 | PASS: resumes 3.6 s after the network returned, output during the gap exactly once | container eth0: sent=0, received=313 |
| terminal_matrix N5 server unreachable 2 min | F3-18 | PASS: open terminal answers at 30/60/90/120 s | container eth0 to 157.180.74.88: sent=0 |
| direct_path_cut (stream rwc-l -> winpool-2, direct path dropped silently 30 s) | F21-160 | PASS: Path direct before, 132 lines, longest gap 6.5 s (budget 15 s; the 6.5 s trust timeout) | pbvm2: sent=0 to the wire, 49 dropped from rwc-l, rule counters 69 |
| device_paths rebind (peer cut 180 s, WinDivert rewrites the device's UDP source ports) | F21-49 | PASS 9/9: first inbound exec 3.9 s after the return (limit 10 s), 7 rebound flows, next 3 inbound + outbound exec ok | container eth0: sent=0, received=498 |
| device_paths udp443 (UDP blocked except 53/443, daemon restarted) | PR-02 | PASS 5/5 twice: exec peer->W 2.9 s / 3.8 s, W->peer 8.7 s / 8.8 s with the block | see below |
| device_paths relay (relay allowed + 1 MB; direct path dropped, relay allowed) | F21-51 | PASS 4/4: cold exec 6.1 s and 6.6 s (limit 15 s), 1 MB intact | see below |

PR-02 proof (udpprobe runs on winpool-2, datagrams with a marker payload counted at anvit):
- rule as read on the device during the block: `ports=1-52,54-442,444-65535`
- without the block: 443: 5/5, 444: 5/5, 27445: 5/5 arrive. With it: 443: 5/5, 444: 0, 5000: 0, 27445: 0, 27446: 0, 30000: 0.
- pb.exe's own UDP during the block, 25 s from +5 s: 0 at the relays (anvit), 1 packet at the peer host (from the
  daemon's NAT port, 10 s in). That is the same 1-packet residual fix-winpathlabel measured; one outbound datagram
  cannot carry a path. In the PR-02 run itself no UDP from the device reached pbvm2 during the block window.

F21-51 proof: during the direct-dropped case (rule ~23:27:57 to ~23:28:35 UTC) pbvm2 eth0 got no UDP from the device,
while anvit and oracle got its relay UDP (25 packets/s at 23:28:03). The no-relay case (both relays dropped) needs a
same-LAN pair and cannot run on a hosted runner. Its pass on victus (relay-paths-windows-20261112T162650Z.json) is not
affected by the flaw: cold_exec (device_paths.py) restarts both daemons after the rule goes in, so every flow it
measures is new and the rule applies to it.

Runs not counted (harness, fixed before the counted run): N5 first try (container_cut.sh passed an IPv4 address to
ip6tables); rebind 231409Z/231813Z (the capture counted inbound IPv6 / ARP and packets after the rules were lifted);
direct_path_cut 232225Z (EPERM cut, see above). Results (local): terminal N4/N5 log, rebind-windows-20261116T232209Z.json,
direct-path-cut-windows-20261116T232518Z.json, udp-blocked-windows-20261116T225754Z.json and 225917Z, relay-paths-windows-20261116T232839Z.json.
