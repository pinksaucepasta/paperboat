#!/usr/bin/env python3
"""F21-162: how long exec stops while `pb install` runs over an existing install (staging containers, never prod).

rel-sco-a starts one non-retrying `pb exec rel-sco-b -- true` every 0.5 s (each its own process, capped at 12 s, so a slow
relayed exec does not set the cadence); while it runs, rel-sco-b reinstalls its own pb binary over the install. The dark window is the
longest stretch with no probe completing (the method of the Windows measurement in results/w21-windows-cutover-gap-
20261111.json, so the number is comparable). The check passes when the window is at most --budget seconds and there
is exactly one such outage. Containers come from `control_outage.py setup`.

A macOS target (F21-162, M): --target-ssh HOST --target-name ALIAS --probe-ct CONTAINER: the probe runs in CONTAINER on --host
(a Linux machine of the same account) against the Mac ALIAS, and the Mac reinstalls ~/pb-new over its install; the install is
timed on the Mac's clock, the dark window is a gap between probe completions so clock skew does not enter it.
"""
import argparse, base64, json, sys, time
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import control_outage as c  # noqa: E402

PROBE = r'''
import subprocess, sys, threading, time
end = time.time() + float(sys.argv[1])
lock = threading.Lock()
def probe():
    start = time.time()
    try:
        rc = subprocess.run([".local/bin/pb", "exec", sys.argv[2], "--", "true"], capture_output=True, timeout=12).returncode
    except subprocess.TimeoutExpired:
        rc = 124
    with lock:
        print(f"{start:.3f} {time.time():.3f} {rc}", flush=True)
threads = []
while time.time() < end:
    # One probe every 0.5 s, each in its own thread: an exec over a relayed path takes seconds, and the dark window
    # must not be the probe's own cadence.
    thread = threading.Thread(target=probe); thread.start(); threads.append(thread)
    time.sleep(0.5)
for thread in threads:
    thread.join()
'''


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", default="ubuntu@anvit")
    parser.add_argument("--budget", type=float, default=8.0, help="seconds without a completing exec (Linux baseline 3.7 s)")
    parser.add_argument("--seconds", type=int, default=90)
    parser.add_argument("--target-ssh", help="ssh host of a macOS target that runs ~/pb-new install (default: the rel-sco-b container)")
    parser.add_argument("--target-name", default=c.NAMES["b"])
    parser.add_argument("--probe-ct", default=c.NAMES["a"])
    args = parser.parse_args()
    host = args.host
    probe_ct = args.probe_ct
    probe_script = "/home/pbt/update_probe.py"
    encoded = base64.b64encode(PROBE.encode()).decode()
    c.in_ct(host, probe_ct, f"echo {encoded} | base64 -d > {probe_script}", 30)
    c.detached(host, probe_ct, f"rm -f /tmp/update-probe.log; python3 {probe_script} {args.seconds} {args.target_name} < /dev/null > /tmp/update-probe.log 2>&1")
    time.sleep(20)  # warm: the probe completes before the install starts
    # The install is timed on the container's clock, the same clock the probe log uses.
    if args.target_ssh:
        stamp = "python3 -c 'import time;print(time.time())'"
        rc, out = c.host_run(args.target_ssh, f"{stamp}; ~/pb-new install >/dev/null 2>&1; echo rc=$?; {stamp}", 330)
    else:
        rc, out = c.in_ct(host, c.NAMES["b"], 'date +%s.%N; ./pb install >/dev/null 2>&1; echo rc=$?; date +%s.%N', 300)
    lines = out.split()
    started, finished = float(lines[0]), float(lines[-1])
    rc = int(next(x for x in lines if x.startswith("rc="))[3:])
    time.sleep(args.seconds - 20 - (finished - started) + 20)
    rc2, log = c.in_ct(host, probe_ct, "cat /tmp/update-probe.log", 30)
    rows = [line.split() for line in log.strip().splitlines() if len(line.split()) == 3]
    rows = sorted((float(a), float(b), int(r)) for a, b, r in rows)  # by start; overlapping probes finish out of order
    rows.sort(key=lambda row: row[1])
    ok_ends = [end for _, end, rc_ in rows if rc_ == 0]
    gaps = [(later - earlier, earlier) for earlier, later in zip(ok_ends, ok_ends[1:])]
    dark, at = max(gaps, default=(0.0, 0.0))
    outages = sum(1 for gap, _ in gaps if gap > 3.0)
    before = sum(1 for _, end, rc_ in rows if rc_ == 0 and end < started)
    quiet = [gap for (gap, at_) in gaps if at_ < started - 1]  # gaps before the install began: the probe's own cadence
    result = {
        "check": "update-outage", "install_rc": rc, "install_seconds": round(finished - started, 1),
        "probes": len(rows), "failed_probes": sum(1 for r in rows if r[2] != 0), "completed_before_install": before,
        "probe_gap_before_install_max_s": round(max(quiet, default=0.0), 1), "gaps_s": [round(g, 1) for g, _ in gaps],
        "dark_window_s": round(dark, 1), "dark_began_s_after_install_start": round(at - started, 1), "outages_over_3s": outages,
        "budget_s": args.budget,
        "status": "PASS" if rc == 0 and before >= 5 and dark <= args.budget and outages <= 1 else "FAIL",
    }
    out_dir = HERE / "results"
    out_dir.mkdir(exist_ok=True)
    path = out_dir / f"update-outage-{time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())}.json"
    path.write_text(json.dumps(result, indent=1) + "\n")
    print(json.dumps(result))
    sys.exit(0 if result["status"] == "PASS" else 1)


if __name__ == "__main__":
    main()
