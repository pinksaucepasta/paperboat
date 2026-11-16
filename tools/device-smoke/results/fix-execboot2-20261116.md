# FIX-EXECBOOT2: first exec after a daemon restart with Paperboat unreachable (2026-11-16)

pbvm2 containers fexb2-a (host under test, machine_a225bc09) and fexb2-b (caller, machine_e0af6e0f), Ubuntu 24.04
systemd; API dropped for both container IPs in DOCKER-USER; `systemctl --user restart paperboatd` on fexb2-a, then
`pb exec fexb2-a -- echo ok` from fexb2-b. The account's ENV lookup for these machines answers "pending" (no vault
delivery), so every run prints an ENV notice; the fix keeps that answer as the last-known one.

| case (b->a) | dev-4436aed05 (before) | dev-745f17c53 (after) |
|---|---|---|
| daemon restart, outage: first exec, run 1 / 2 / 3 | 5.37 / 0.39 / 0.38 s | 1.37 / 1.38 / 1.36 s |
| same, second exec | 0.36 / 0.37 / 0.38 s | 1.38 / 1.38 / 1.35 s |
| daemon restart, online: first exec | 0.36 / 0.38 s | 0.36 / 0.36 s |
Runs 2-3 "before" were fast only because run 1's 5 s failure had just marked the control plane down (30 s window); a
first exec after a fresh silent cut always waited 5 s. After: at most 1 s (cached-answer budget) plus the exec, with
the kept "pending" notice instead of "offline".

Rate check, 5 min of fexb2-a's API flapping 30 s cut / 30 s open, one exec b->a every 20 s, server-side
`msg=http_request ... client=paperboat/<build>` lines from the prod server log, per support_reference:
| fexb2-a process | before (17:47:32-17:52:34Z) | after (18:05:08-18:10:20Z) |
|---|---|---|
| ENV layer lookups (`/v1/environment/hosts/A/layers`) | 9 | 7 |
| host runtime (machine-events/wait, observations, audit-backlog...) | 69 (39 waits) | 81 (50 waits) |
| carrier (machine-peer-network/config, audit-backlog) | 16 | 20 |
The change touches only the ENV lookups, which went down (shared in-flight request, no request while known down).
The other differences are long-poll timing against the cut phases: fexb2-b, which was never cut and runs the same
unchanged code, went 48 -> 83 requests between the two windows. No 429, no new request type.

Pair check on dev-745f17c53: `pb exec` b->a and a->b PASS (hostname), `pb connect fexb2-a new` from b PASS (echoed
CONNECT_42). CI: tools/ci-test.sh cli -run 'TestLayer|TestConcurrentLayer|TestEnvironment|TestControlWatch|
TestCreateWithoutARecordTerminatesTheShell|TestManager|TestCreate' success at 745f17c53 (run 43922906252).

Not done: a second exec within the first request's 5 s still waits its own 1 s (the budget is per launch, not from the
request's start); the kept answer is in memory only, so a restarted host process still uses the 2 s unproven budget.
