#!/usr/bin/env python3
"""F21-55 / F21-57 checks of the multi stage: usage multi_check.py WORKDIR BOUND_SECONDS KILLED_EPOCH VICTIM.

1. No exec failed for longer than BOUND seconds (exec.log: "start end ok|fail" per attempt).
2. Leader-only jobs: every "worker iteration" line (server<N>.log) is one run. Before the kill all runs of a
   job come from one instance; afterwards runs of a job are spaced at least half its interval apart,
   except one handover from the killed instance to its successor.
"""
import collections, datetime, re, sys

work, bound, killed, victim = sys.argv[1], float(sys.argv[2]), float(sys.argv[3]), int(sys.argv[4])
fail = []

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
if not runs:
    fail.append("no worker iteration logged")
for job, items in sorted(runs.items()):
    items.sort()
    before = {n for when, n in items if when < killed}
    if len(before) > 1:
        fail.append(f"{job}: ran on instances {sorted(before)} before the kill")
    handovers = 0
    for (t1, n1), (t2, n2) in zip(items, items[1:]):
        if t2 - t1 >= interval[job] / 2:
            continue
        if n1 == victim != n2 and not handovers:
            handovers = 1
        else:
            fail.append(f"{job}: runs {t2 - t1:.2f}s apart on instances {n1},{n2} (interval {interval[job]:.0f}s)")
    print(f"{job}: {len(items)} runs, interval {interval[job]:.0f}s, instances {sorted({n for _, n in items})}")

for line in fail:
    print("FAIL", line)
sys.exit(1 if fail else 0)
