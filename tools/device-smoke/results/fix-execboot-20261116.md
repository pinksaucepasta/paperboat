# FIX-EXECBOOT: first pb exec after a host restart, online and with Paperboat unreachable (2026-11-16)

hp containers fexb-a (host under test) and fexb-b (caller), Ubuntu 24.04 systemd, API dropped for both container IPs in
DOCKER-USER (the container's own iptables would not survive `docker restart`). First exec 4 s (daemon restart) or 4-8 s
(container restart) after the restart returns; later execs 0.38-0.42 s. "ENV not applied" notice in every outage run (expected).

Timeline of the original 32 s (host restart, API dropped, ms trace; dev-69e14a849): carrier `allowPendingPeerEnrollment` waited the
10 s control dial before admitting anything; the stable daemon starts its components in order, so `preview_recovery` (15 s) and
`runtime_observation` (availability resolve 10 s + first observation 10 s) held back `control_plane`, where the admitted exec is
served (exec Start at +35 s). After those two fixes the ENV lookup waited 15 s behind the boot registration's `refresh` mutex
(trace dev-d0016292d). On the caller side, `Authority.RegisterOrRestore` waited the full 10-15 s register request before restoring
(trace of the local daemon's HTTP requests, dev-8688a50b4).

| case | build | before | after (dev-bf7b986c3) |
|---|---|---|---|
| host restart (container), outage: first exec b->a | main + c5e7bd948 | 31.8 s | 2.39 s |
| host restart (container), outage: first exec a->b (restarted machine is the caller) | main | 10.4 s fail | 2.88 s |
| daemon restart, outage, b->a / a->b | main | 12.7 s / 10.4 s fail | 7.65 s / 2.63 s |
| daemon restart, online, b->a / a->b | | 1.14 s / 1.90 s | 1.14 s / 2.39 s |
Daemon restart in an outage leaves the host's worker warm with a silently dead HTTP/2 connection: the first exec after the cut waits
for its ENV lookup (budget 5 s, was 15 s); not under 3 s. Time-to-reachable after `docker restart` (container boot included):
online 8-10 s, outage 10.3 s (was 13.5 s); that is systemd and service start, not control-plane waits.

Not done: skipping the ENV lookup when the host knows the machine has no layers needs a server push (no machine-events topic
announces ENV changes; Spark trace: server/internal/environment publishes nothing) and a host-side negative cache; Lane B work.
Orphan terminal after pair (R-F21-164B) did not reproduce on hp (pair then `pb new` at once; `pb new` after a daemon restart with
the API dropped: no stray shell); two verified create-path cleanup gaps fixed instead.
Client-side waits named by the audit: ControlPlaneReachable is not called by exec; the readiness poll is connect-only; the 30 s
credential refresh and 30 s target-list call are reached only with a cold daemon snapshot and a due token (not hit in these runs).
