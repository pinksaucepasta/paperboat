# FIX-EXECOUTAGE: pb exec in a control-plane outage, and managed ENV on pb exec (2026-11-16)

hp containers eo-a, eo-b (Ubuntu 24.04 systemd, tester, paired to the hp test account), API cut = iptables DROP of the API address in
DOCKER-USER for both container IPs only; `pb exec` runs `sh -c 'echo v=${EO_TEST_VAR:-unset}'` with EO_TEST_VAR set in the personal
scope and the vault unlocked on both.

## Regression found on the way: no pb exec got managed ENV since b79fb1520
`pb env host show` said published+applied, vault unlocked, yet every pb exec printed "ENV ... delivery to this machine is pending"
and the variable was unset (dev-763a4e11d). Debug build log: `NOTREADY at environment_layers.go:69 ok=false nil=false closed=false`.
Cause: offlineAuthorizer (the standing-rule admission) built the Authorization without WorkspaceID, so the launch context was invalid.
Fix c5e7bd948 (binds the personal workspace; the server refuses personal unless the actor owns the machine,
server/internal/environment/vault_layers.go:329, and the host again at environment_layers.go:96). Online after the fix: `v=x`, no notice.
Known limit: a team-owned machine gets its owner's personal ENV, not the team's (needs the team id in the stream header).

## Outage timing, ENV delivered, eo-a restarted (docker restart) with the API cut; exec 20 s later
| build | eo-b->eo-a (first) | eo-a->eo-b | eo-b->eo-a | eo-a->eo-b |
|---|---|---|---|---|
| dev-2b0966c8f (main + ENV fix only) BEFORE | 30.8 s | 15.7 s | 15.4 s | 15.4 s |
| dev-bc3734e06 (watch inside backoff, never matched) | 36.3 s | 15.7 s | 15.4 s | 15.4 s |
| dev-96b52a82f (this change) AFTER | 21.1 s | 5.2 s | 0.37 s | 0.37 s |
Online (no cut) on the same builds: 2-4 s, first exec after an install/restart up to 10.7 s (cold start, not the outage).
Every outage exec printed "Paperboat ENV not applied: ENV needs Paperboat online..." and ran.
Not fixed: the first exec after a restart in an outage still waits one ENV lookup (21 s here, of which ~10 s is the online cold start);
the host marks the outage after its own first failed control request.
