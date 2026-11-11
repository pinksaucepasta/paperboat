#!/usr/bin/env python3
"""F21-162: how long exec stops while `pb install` runs over an existing install (staging containers, never prod).

rel-sco-a runs one non-retrying `pb exec rel-sco-b -- true` per second (each capped at 8 s, so a probe never
overlaps the next); while it runs, rel-sco-b reinstalls its own pb binary over the install. The dark window is the
longest stretch with no probe completing (the method of the Windows measurement in results/w21-windows-cutover-gap-
20261111.json, so the number is comparable). The check passes when the window is at most --budget seconds and there
is exactly one such outage. Containers come from `control_outage.py setup`.
"""
import argparse, base64, json, sys, time
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import control_outage as c  # noqa: E402

PROBE = r'''
import subprocess, sys, time
end = time.time() + float(sys.argv[1])
while time.time() < end:
    start = time.time()
    try:
        rc = subprocess.run([".local/bin/pb", "exec", sys.argv[2], "--", "true"], capture_output=True, timeout=8).returncode
    except subprocess.TimeoutExpired:
        rc = 124
    print(f"{start:.3f} {time.time():.3f} {rc}", flush=True)
    time.sleep(max(0, 1 - (time.time() - start)))
'''


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", default="ubuntu@anvit")
    parser.add_argument("--budget", type=float, default=8.0, help="seconds without a completing exec (Linux baseline 3.7 s)")
    parser.add_argument("--seconds", type=int, default=90)
    args = parser.parse_args()
    host = args.host
    probe_script = "/home/pbt/update_probe.py"
    encoded = base64.b64encode(PROBE.encode()).decode()
    c.in_ct(host, c.NAMES["a"], f"echo {encoded} | base64 -d > {probe_script}", 30)
    c.detached(host, c.NAMES["a"], f"rm -f /tmp/update-probe.log; python3 {probe_script} {args.seconds} {c.NAMES['b']} < /dev/null > /tmp/update-probe.log 2>&1")
    time.sleep(20)  # warm: the probe completes before the install starts
    # The install is timed on the container's clock, the same clock the probe log uses.
    rc, out = c.in_ct(host, c.NAMES["b"], 'date +%s.%N; ./pb install >/dev/null 2>&1; echo rc=$?; date +%s.%N', 300)
    lines = out.split()
    started, finished = float(lines[0]), float(lines[-1])
    rc = int(next(x for x in lines if x.startswith("rc="))[3:])
    time.sleep(args.seconds - 20 - (finished - started) + 5)
    rc2, log = c.in_ct(host, c.NAMES["a"], "cat /tmp/update-probe.log", 30)
    rows = [line.split() for line in log.strip().splitlines() if len(line.split()) == 3]
    rows = [(float(a), float(b), int(r)) for a, b, r in rows]
    ok_ends = [end for _, end, rc_ in rows if rc_ == 0]
    gaps = [(later - earlier, earlier) for earlier, later in zip(ok_ends, ok_ends[1:])]
    dark, at = max(gaps, default=(0.0, 0.0))
    outages = sum(1 for gap, _ in gaps if gap > 3.0)
    before = sum(1 for _, end, rc_ in rows if rc_ == 0 and end < started)
    result = {
        "check": "update-outage", "install_rc": rc, "install_seconds": round(finished - started, 1),
        "probes": len(rows), "failed_probes": sum(1 for r in rows if r[2] != 0), "completed_before_install": before,
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
