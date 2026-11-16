# PR-03 idle budget, Windows, on prod server 2026.11.13.48 (up since 21:22 UTC), 2026-11-17 (IST 03:06-03:21)

Windows build `da6c3f30b` = main's PR-03 fixes (1b76eaaee, 4fdc7707e, f6d974364, 4d4aba888's change) on a base before
10b5adaea (version string patched to `dev-pr03w0027` for server-side attribution), winpool-1 (GitHub Windows runner),
paired in a fresh throwaway account with L (`dev-pr03l0024`) and M (`dev-pr03m0024`); one 15-minute window
(21:36:00-21:51:10 UTC), machines settled 40 min (Windows 14 min after its last install).
Server counts: `client=paperboat/<version>` lines of the server access log, bytes: its `request_bytes`/`response_bytes`
(bodies only; "+hdr" adds 600 B per request).

| | requests / 910 s | req/s (<= 0.1) | API MB/h body (+hdr) (<= 5) | CPU of all pb (<= 1%) | wakeups/s |
|---|---|---|---|---|---|
| W | 85 | 0.093 PASS | 0.72 (0.92) PASS | 0.72% PASS | 121 |
| L | 76 | 0.084 PASS | 0.64 (0.82) PASS | 0.77% PASS | 131 |
| M | 89 | 0.098 PASS | 0.93 (1.14) PASS | 0.68% PASS | 95 |

Windows root cause: the hostd worker polled its supervisor pipe with PeekNamedPipe every 20 ms for its whole life
(worker_activation_windows.go): 262 context switches/s in the worker, 94 in the updater, against 98/s for a whole macOS machine.
Poll now 20 ms for 2 s, then 250 ms: worker 87/s right after install, whole machine 121 wakeups/s; same runner before the
fix read 1.25-1.69% CPU and 318-358 wakeups/s. Earlier fixes on the way: Windows port scan asks each pid once, runtime identity
reused 10 s, alias domain cached, heartbeat 15 s, port scan 15 s, managed SSH gate poll 5 s.
