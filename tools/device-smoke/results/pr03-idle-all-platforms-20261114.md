# PR-03 idle machine budget, L/M/W, staging (2026-11-14 .. 11-16 IST)

Each machine ran a build with a unique version string (binary-patched, so the server log counts its requests by
`client=paperboat/<version>`), idle, 600 s windows; Linux container on pbvm2, Mac = macpool runner, Windows = winpool
runner. Budget: <= 1% of one core, <= 600 wakeups/s, <= 5 MB/h API bytes, <= 0.1 req/s.

| build | platform | CPU % | wakeups/s | API MB/h | server req/s |
|---|---|---|---|---|---|
| dda3519a0 (before) | Windows | 7.33 | 489 | 41 (whole NIC) | 0.095 |
| dda3519a0 (before) | macOS | 1.18 | 135 | 12.8 (pb sockets, loopback incl.) | 0.097 |
| 8aafdbab9 | Linux | 0.81 | 124 | 0.0 (measurement bug, below) | 0.092 |
| 8aafdbab9 | macOS | 1.22 | 131 | 14.0 | 0.1015 |
| 8aafdbab9 | Windows | 0.93 | 368 | 40 (whole NIC) | 0.095 |
| e6f72ff83, quiet account usr_pr03 | Linux | 0.69 | 97 | 3.99 | 0.112 |
| e6f72ff83, quiet account usr_pr03 | macOS | 1.07 | 98 | 4.19 (remote TCP of pb) | 0.117 |
| e6f72ff83, quiet account usr_pr03 | Windows | 1.32 | 359 | n/a (NIC 24) | 0.120 |

Findings fixed: Windows updater re-hashed pb.exe twice per 5 s pass (Digest cache, 15f0b3533, 76abb43cf; 75 MB/s
-> 0.7 MB per 30 s), Windows/macOS updater pass 15 s and port watch 5/10 s (8aafdbab9, 4dc82e483), perf_idle.py counted
only remote port 443 (staging is 18443: 0.0 MB/h while the container moved 16 MB/h) (e6f72ff83),
idle_api_requests.sh read 0 on an empty log, 509793a3d events/wait hold 55 s (24 -> ~11 requests per 10 min; needs the
server deploy). The shared staging account (usr_staging, ~100 machines) inflates API bytes (peer lists); the quiet
account usr_pr03 does not. Still open: macOS 1.07% and Windows 1.32% CPU, the CLI-role peer-network/config renewal
(~11-14 per 10 min in the quiet account, ~0-1 in the shared one; cause not found), carrier renewal 1 min (design, 46aa1e11).
