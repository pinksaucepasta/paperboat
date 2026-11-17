# F3-15 M: terminal 60 s black hole rerun with the cut on the Linux peer (lane FLEET4, 2026-11-17 05:40-05:50 IST)

Why: RERUN-SILENTCUT (c496db157) found a local macOS pf cut does not measure a dead path (the Mac leaves the direct path at once). The earlier M N13 evidence used pf.

Machines, build dev-c496db157 (main c496db157) on both: macpool-1 (GitHub-hosted macOS 26.7.2 arm64, machine fsc-m, the terminal client) and fsc-t (Linux container on pbvm2, the session host), same production test account. `pb status` on the Mac: Path direct to fsc-t before the run.

Cut: `terminal_matrix.py fsc-t --only blackhole` run on the Mac with SMOKE_PEER_CUT = a helper that, for `add all`, starts winhost.linux_silent_cut(['!','-o','lo'], ['!','-i','lo']) inside fsc-t for 60 s via `pb exec` (3 s after the exec returns; outgoing packets are marked and lost in a netem `loss 100%` qdisc, so every send succeeds and nothing reaches the wire; incoming ones dropped in INPUT) and for `remove` waits for the lift and prints the proof line.

```
PASS N13 black hole 60 s: the client shows it is reconnecting within 30 s: 14.2 s
peer cut (Linux fsc-t): cut 1794874476.662 lift 1794874536.662 silent cut on eth0: 379 outgoing packets lost after their send succeeded, 302 incoming dropped
PASS N13 black hole 60 s: session resumes on its own: first output 1.8 s after the network returned
PASS N13 black hole 60 s: output during the gap arrives exactly once
PASS N13 black hole 60 s: keys typed meanwhile ran exactly once: 1 times
```
Two earlier runs with the same cut: 14.1 s / first output 0.7 s / exactly once / keys once (all PASS). (A first attempt failed in my helper, macOS has no `timeout`; no cut was made.)

Cut proven by a capture in fsc-t's network namespace on pbvm2 (`tcpdump -nni eth0`, IP, not DNS): packets sent by fsc-t per second relative to the cut start: 24, 5, 3 before it, then 0 for every second 1..59, 2 at +60, 79 at +61 (resumed); packets received from the Mac inside the cut (+1..+59): 299, so the Mac kept sending while nothing came back, a silent dead path. Cut window 60.0 s.

Counted run: the third (it has the capture). Fixtures removed afterwards (container, macpool-1, records fsc-t and fsc-m revoked by exact id).
