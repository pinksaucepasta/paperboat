#!/usr/bin/env python3
"""F21-166: 32 concurrent `pb exec` from one machine to one peer over the network, each running `sleep 20`.

Run on the staging containers (never prod) from the orchestrating host:
  exec_scale.py --host ubuntu@anvit [--count 32] [--sleep 20] [--budget 30]
rel-sco-a starts COUNT execs at once to rel-sco-b (containers from control_outage.py setup); each must exit 0 within
sleep + (budget - sleep) seconds. The result JSON has the start spread, the slowest and median times and every failure.
"""
import argparse, base64, json, sys, time
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import control_outage as c  # noqa: E402

WORKER = r'''
import json, subprocess, sys, threading, time
count, sleep_s, target = int(sys.argv[1]), sys.argv[2], sys.argv[3]
rows = [None] * count
def run(i):
    start = time.time()
    done = subprocess.run([".local/bin/pb", "exec", target, "--", "sleep", sleep_s], capture_output=True, text=True, timeout=300)
    rows[i] = {"i": i, "seconds": round(time.time() - start, 2), "rc": done.returncode, "tail": (done.stdout + done.stderr).strip()[-160:]}
gate = threading.Barrier(count)
def go(i):
    gate.wait()
    run(i)
threads = [threading.Thread(target=go, args=(i,)) for i in range(count)]
[t.start() for t in threads]; [t.join() for t in threads]
print(json.dumps(rows))
'''


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", default="ubuntu@anvit")
    parser.add_argument("--count", type=int, default=32)
    parser.add_argument("--sleep", type=int, default=20)
    parser.add_argument("--budget", type=float, default=30.0, help="seconds each exec may take in all")
    args = parser.parse_args()
    encoded = base64.b64encode(WORKER.encode()).decode()
    c.in_ct(args.host, c.NAMES["a"], f"echo {encoded} | base64 -d > /tmp/exec_scale_worker.py", 30)
    rc, out = c.in_ct(args.host, c.NAMES["a"], f"python3 /tmp/exec_scale_worker.py {args.count} {args.sleep} {c.NAMES['b']}", 400)
    try:
        rows = json.loads(out.strip().splitlines()[-1])
    except (ValueError, IndexError):
        print(json.dumps({"check": "exec-scale", "status": "FAIL", "detail": out[-300:]}))
        sys.exit(1)
    times = sorted(r["seconds"] for r in rows)
    failed = [r for r in rows if r["rc"] != 0]
    slow = [r for r in rows if r["seconds"] > args.budget]
    result = {"check": "exec-scale", "count": args.count, "sleep_s": args.sleep, "budget_s": args.budget,
              "fastest_s": times[0], "median_s": times[len(times) // 2], "slowest_s": times[-1],
              "failed": [{"i": r["i"], "rc": r["rc"], "tail": r["tail"]} for r in failed][:8], "over_budget": len(slow),
              "status": "PASS" if not failed and not slow else "FAIL"}
    out_dir = HERE / "results"
    out_dir.mkdir(exist_ok=True)
    (out_dir / f"exec-scale-{time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())}.json").write_text(json.dumps({**result, "rows": rows}, indent=1) + "\n")
    print(json.dumps(result))
    sys.exit(0 if result["status"] == "PASS" else 1)


if __name__ == "__main__":
    main()
