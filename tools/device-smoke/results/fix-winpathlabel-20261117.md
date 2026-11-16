# FIX-WINPATHLABEL: Windows firewall "block" did not block, 2026-11-17 IST

Machines: winpool-2 (hosted Windows 11 runner) and a pbvm2 container, both dev-9f761e9cf (before 8200fbeb1), paired.
Counts: `tcpdump -ni any udp and host <W public IP>` on the pbvm2 host, 30 s windows, pings both ways every ~2 s.

Finding: the `path=direct` label was correct. `pb ping` reads magicsock's trusted best address after a dial
(mesh/regional_status.go PeerPaths); trust ends 6.5 s after the last pong, so it cannot stay `direct` on a dead path.
The harness was wrong: Windows Firewall Block rules are applied when a socket first uses a flow, so a pb.exe that
already talks to the peer keeps sending and receiving. During the "block" Linux->Windows connect time stayed 235 ms (direct).

| block | packets W->L per 30 s | to-W |
|---|---|---|
| none | 430-442 | 426-450 |
| machine-wide rule (UDP, peer IP, both directions), pb active | 193 | 264 |
| program-scoped rule (the old NET_CUT form) | 133 | 180 |
| same rule, nothing active at block time (20 s window) | 0 | - |
| rule + stop pb.exe that own sockets (services restart them) | 1 | 45 |
| fixed harness `WindowsDevice.cut_network(addresses=[peer])`, 20 s after | 1 | 21 |

Fix: winhost.NET_CUT and UDP_BLOCK stop the pb.exe that own sockets the cut covers right after adding the rule.

Checks that reach NET_CUT / UDP_BLOCK on Windows (Spark list, verdict A = a PASS could be false because nothing was
blocked; B = fails honestly when not blocked; C = unaffected). Rows to rerun first: the A ones.
| check | rows / evidence | verdict |
|---|---|---|
| network_drop.py (F3-03) | windows-20261110T232634Z.json | B (needs failures_since>0) |
| terminal_matrix N4 network drop | F3-15/16, windows-20261110T084227Z.json | A |
| terminal_matrix N13 black hole | F3-15 | B |
| terminal_matrix N5 control plane down | F3-18 | A |
| net_blip.py | F21-48 | B |
| control_outage_windows.py | F21-01/F21-11 | B (survival-only rows A) |
| direct_path_cut.py | F21-160 Windows twin | A |
| dead_path_stream.py / relay_home_wire.py | F21-160, F21-161 | B |
| path_display.py | F21-06 | B |
| transfer_modes_windows.py | F22-01 | B |
| usage_categories.py | F13-02 | B |
| private_access.py / local_access.py path-relay | F9-05 | B |
| device_paths.py Windows (rebind, relay paths, PR-02 udp 53/443) | F21-49, F21-51, PR-02 | A |
| test_winhost.py cut unit | - | C |
Not covered by the fix: UDP_BLOCK ignores the `allow` ports and always lets only DNS through (winhost.py UDP_BLOCK), so PR-02's "except 53/443" is not what it blocks.
