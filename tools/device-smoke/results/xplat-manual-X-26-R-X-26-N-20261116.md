# X-26 sleep / wake by freeze + network cut (lane R-X-26-N, 2026-11-16)

Stand-in for sleep (user decision 2026-11-16): freeze every pb process of the sleeping machine (SIGSTOP on Linux/macOS,
NtSuspendProcess on Windows) AND cut its pb network for 240 s (Linux: iptables owner drop of the pb user; macOS: pf anchor
`block drop out quick on en0 all user admin`; Windows: firewall block of every pb.exe, both directions), then resume both.
The machine is set to allow-sleep (Linux, Windows; the fleet Mac's own mode is left alone). Linux sleeper: systemd's
WatchdogSec is set to 0 on the paperboat units, because SIGSTOP trips the 30 s watchdog and systemd aborts the frozen daemon
(a real sleep stops that clock). Scripts: scratchpad driver, not committed (freeze/thaw + one run per sleeper).

Builds: dev-caf37d9c6 (main, contains the preview fix c5362743f) on all: L = Linux container rx26n-l (pbvm2), W = winpool-3
(rx26n-w, hosted Windows runner), M = the fleet Mac lanex2-m with the fleet Linux lanex2-l2 as its peer (FLEET4 installed
caf37d9c6). Each run: terminals open to and from the sleeper (typed before, typed WHILE frozen, typed after), a plain
`pb preview --background` (no --ttl) and a durable tunnel on the sleeper, a send in flight to and from it, a held-session reattach.

| Sleeper | offline + reason while away | exec refused clearly | exec back | terminals (typed while frozen ran once) | tunnel | preview (plain, no --ttl) | reattach held session | sends in flight | fresh send |
|---|---|---|---|---|---|---|---|---|---|
| L (peer W) 12:46Z | pass, 78 s "offline (allowed to sleep)" | exec returned after 40 s timeout (other runs: clear error) | 3-6 s | pass, back 8 s | 5 s | 35 s | pass | both pass | pass |
| W (peer L) 12:26Z | pass, 108 s | pass | 9-11 s | pass, back 13-17 s | 14 s | 78 s | first immediate key lost its Enter, typed 8 s later passes | both pass | pass |
| M fleet (peer L2) 12:50Z | pass, 104 s | pass | 2-3 s | pass, back 4-5 s | 7 s | 18 s | pass | L2>M failed once (see failures.md), M>L2 pass | pass |

Product bugs found and fixed here: c5362743f: after a wake the first preview renewals are refused 403 (host presence not yet
online) or 412 (readiness moved the generation), the renew loop treated that as lease loss and stopped the preview (lost in 3 of 7
runs before; test TestRenewalSurvivesOwnerRefusalsUntilThePresenceReturns, CI cli -race green). Before it, recoveries were
15-63 s or never; with it 18-78 s in 5 of 5 runs.

Open (not ticked): an in-flight `pb send` failed once when the Linux sender was the frozen side (file_transfer_unavailable)
and once when the macOS receiver was frozen (HTTP 499 at +115 s); 7 other in-flight sends resumed and published. Windows
reattach: Enter typed in the first 0.5 s after attach is dropped. Both in failures.md.
