# RERUN-SILENTCUT: dead-path and black-hole checks rerun with a silent cut, 2026-11-17 IST

Why (RERUN-WINCUT, 984c73785): an iptables DROP in the machine's own OUTPUT chain fails sendmsg with EPERM, and magicsock
rebinds at once (magicsock_notplan9.go:29). A check cut that way measures the ~1 s fast path, not a real black hole
(~6.5 s: 3 s heartbeat, 6.5 s trust). Suspect: Linux dead_path_stream.py, Linux terminal_matrix N13, F21-160 M evidence
(dead_path_stream on the Mac, gap 0.2 s).

Machines: containers rsc-a, rsc-b (machine rsc-b-2) on pbvm2 (~/laneh mkct.sh), macpool-2 (GitHub-hosted macOS arm64,
machine rsc-m). All `pb version` dev-b112b2a4a (the 04:00 IST build-hold build), production test account.

## The cut now (harness change in this commit)

- Linux default (winhost.linux_silent_cut, used by terminal_matrix drop_all, path_display.udp_blocked,
  relay_home_wire.blocked): outgoing packets are marked in mangle OUTPUT and lost in a netem `loss 100%` qdisc on the
  default-route interfaces, incoming ones dropped in INPUT. Proof by hand in rsc-a: python sendto 5/5 succeed, netem
  dropped 5, host capture on rsc-a eth0 0 packets. A 40 s cut with an outbound-only capture (`tcpdump -Q out`): 0 frames
  of any kind left between cut and lift. The cut prints its own proof line (packets lost after a successful send, incoming dropped).
- macOS pf `block drop out` is already silent for UDP: on macpool-2 sendto 5/5 succeed under the rule and 0 reach en0
  (pf rule counter 5). Kept as is.

## Results

| check | row cell | result | cut proven by |
|---|---|---|---|
| dead_path_stream rsc-a -> rsc-b-2 (UDP except 53, silent) | (no L tick uses it; L F21-160 is direct_path.py, already a silent DOCKER-USER drop) | PASS: longest gap 6.0 s (bound 12; the EPERM cut gave 0.8 s), 33 lines before, 442 after, in order, path wss while blocked | proof line: 214 outgoing lost after send succeeded, 226 incoming dropped; host capture: 0 UDP from rsc-a on the wire after the cut |
| terminal_matrix N13 black hole 60 s, rsc-a -> rsc-b-2 | F3-15 L | PASS twice. Counted run: reconnecting shown 11.2 s, first output 3.2 s after the return, output exactly once, keys once (first run: 11.2 s, 4.1 s, all PASS) | proof line: 474 lost, 339 incoming dropped; host capture (IP): 0 packets from rsc-a for the whole 60 s window |
| terminal_matrix N4 network drop 45 s, rsc-a -> rsc-b-2 | F3-15 L (whole row) | PASS: resumed 9.0 s after the return, output exactly once | proof line: 331 lost, 339 incoming dropped; host capture (IP): 0 packets from rsc-a during the 45 s |
| F21-160 on the Mac, old way: dead_path_stream on macpool-2 -> rsc-b-2 (pf, proven silent) | F21-160 M | gap 0.1 s again, path wss: the Mac leaves the direct path at once even though no send fails, so a local pf cut does not measure the silent case on macOS (not counted) | en0 capture: 0 UDP out during the block |
| F21-160 on the Mac, peer-side silent cut: stream `pb exec rsc-b-2` on macpool-2 (0.1 s lines, timestamped on the Mac), linux_silent_cut in rsc-b to/from the Mac's public address for 40 s | F21-160 M | PASS: Path direct before and after, longest gap 4.8 s right at the cut (bound 12), 739 lines in order | rsc-b eth0 capture during the cut: sent=0, received=36 (the Mac's sends arrive, so none failed); Mac en0: data UDP to pbvm2 ~11/s until the cut, then only probes every 5 s until the lift; proof line 134 lost, 39 dropped |

Clock note: the Mac stream times are the Mac's clock, the cut times pbvm2's (both NTP); the Mac capture's last data
packet before the gap is in the same second as the cut.

Runs not counted: the first Mac orchestration (its start step read the rest of the script from stdin and hung; no
stream ran); dead_path_stream with rsc-b as the client (rsc-b showed `Path: relay/india` to rsc-m while the Mac showed
direct, so the check refused to start). Scripts: RERUN-SILENTCUT-run/{dps,n13,m-start,m-end,c-hold,orch}.sh (local).
