#!/usr/bin/env python3
"""F21-55 / F21-57 checks of the multi stage: usage multi_check.py WORKDIR BOUND_SECONDS KILLED_EPOCH VICTIM.

1. No exec failed for longer than BOUND seconds (exec.log: "start end ok|fail" per attempt).
2. VICTIM was the worker leader when it was killed. Another instance takes over ("worker leader acquired")
   within HANDOVER seconds (leader Check + statement timeout + Retry, workers/leader.go, plus slack).
3. Leader-only jobs: every "worker iteration" line (server<N>.log) is one run. Before the kill all runs of a
   job come from the victim; afterwards runs of a job are spaced at least half its interval apart, except one
   handover from the victim to its successor, and every job runs again on a survivor after the handover.
   LISTENERS (per-process LISTEN loops) must run on every instance and are exempt from the rest.
   REQUIRED jobs must all be seen; the others are loops this CI configuration does not enable (Stripe,
   certificate and DNS providers, WorkOS organizations): TestEveryLeaderWorkerIsRegisteredForIterationCounts
   covers that they are countable.
"""
import collections, datetime, re, sys

work, bound, killed, victim = sys.argv[1], float(sys.argv[2]), float(sys.argv[3]), int(sys.argv[4])
fail = []
HANDOVER = 20
REQUIRED = """worker_machines worker_config_warnings worker_config_revocation worker_preview_leases worker_tunnel_expiry
worker_workspace_domains worker_team_retention worker_transactional_mail worker_team_entitlements
worker_team_inbox worker_edge_sets worker_peer_endpoints worker_selfhost_ingress worker_operational_health
worker_account_deletion worker_audit_streams""".split()
# Per-process LISTEN loops, leader or not (app.go processListeners, F21-04): every instance must run them.
LISTENERS = ["worker_mint_withdrawals", "worker_machine_events"]

start = None
attempts = 0
for line in open(f"{work}/exec.log"):
    begin, end, result = line.split()
    attempts += 1
    if result == "fail" and start is None:
        start = float(begin)
    elif result == "ok" and start is not None:
        if float(end) - start > bound:
            fail.append(f"exec failed for {float(end) - start:.0f}s (bound {bound:.0f}s)")
        start = None
if start is not None:
    fail.append("exec still failing at the end of the run")
if attempts < 20:
    fail.append(f"only {attempts} exec attempts ran")

unit = {"h": 3600, "m": 60, "s": 1}
def seconds(text):
    return sum(float(n) * unit[u] for n, u in re.findall(r"([\d.]+)([hms])", text))

runs = collections.defaultdict(list)  # job -> [(time, instance)]
interval = {}
for n in (1, 2, 3):
    for line in open(f"{work}/server{n}.log", errors="replace"):
        m = re.search(r'time=(\S+) .*msg="worker iteration" worker=(\S+) interval=(\S+)', line)
        if m:
            when = datetime.datetime.fromisoformat(m[1]).timestamp()
            runs[m[2]].append((when, n))
            interval[m[2]] = seconds(m[3])
for job in REQUIRED:
    if job not in runs:
        fail.append(f"{job}: no run logged")
for job in LISTENERS:
    missing = {1, 2, 3} - {n for _, n in runs.pop(job, [])}
    if missing:
        fail.append(f"{job}: never ran on instances {sorted(missing)}")

acquired = []
for n in (1, 2, 3):
    for line in open(f"{work}/server{n}.log", errors="replace"):
        m = re.search(r"time=(\S+) .*msg=\"worker leader acquired\"", line)
        if m:
            acquired.append((datetime.datetime.fromisoformat(m[1]).timestamp(), n))
successors = [(t, n) for t, n in sorted(acquired) if t >= killed - 1 and n != victim]
if not successors:
    fail.append("no instance took over worker leadership")
elif successors[0][0] - killed > HANDOVER:
    fail.append(f"leadership took {successors[0][0] - killed:.0f}s to move (bound {HANDOVER}s)")
else:
    print(f"instance {successors[0][1]} took over after {successors[0][0] - killed:.1f}s")
for job, items in sorted(runs.items()):
    items.sort()
    before = {n for when, n in items if when < killed}
    if before - {victim}:
        fail.append(f"{job}: ran on instances {sorted(before)} before the kill (leader was {victim})")
    handovers = 0
    for (t1, n1), (t2, n2) in zip(items, items[1:]):
        if t2 - t1 >= interval[job] / 2:
            continue
        if n1 == victim != n2 and not handovers:
            handovers = 1
        else:
            fail.append(f"{job}: runs {t2 - t1:.2f}s apart on instances {n1},{n2} (interval {interval[job]:.0f}s)")
    if job in REQUIRED and not any(when > killed and n != victim for when, n in items):
        fail.append(f"{job}: did not run on a survivor after the handover")
    print(f"{job}: {len(items)} runs, interval {interval[job]:.0f}s, instances {sorted({n for _, n in items})}")

for line in fail:
    print("FAIL", line)
sys.exit(1 if fail else 0)
