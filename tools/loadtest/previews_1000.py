#!/usr/bin/env python3
"""F21-168: create N managed previews with the real `pb` CLI, P at a time, against one tiny origin, then delete them.

  previews_1000.py create N P PORT   create N previews (ttl 3h); prints one JSON line per result and a summary
  previews_1000.py total             the server's preview count from one list request
  previews_1000.py delete P          delete every live preview of the account (parallel P)

Run it on the machine that owns the previews (e.g. inside a staging container); sampling the server is the caller's job.
Nothing is retried: the first error of each kind is kept with its support reference."""
import concurrent.futures, json, subprocess, sys, time

PB = "pb"


def run(args, timeout=120):
    return subprocess.run([PB, *args], capture_output=True, text=True, timeout=timeout)


def create(port):
    started = time.time()
    try:
        done = run(["preview", str(port), "--background", "--ttl", "3h", "--json"])
    except subprocess.TimeoutExpired:  # a create that never returned is a result, not a crash
        return {"ok": False, "took": round(time.time() - started, 2), "code": "cli_timeout_120s"}
    took = round(time.time() - started, 2)
    try:
        body = json.loads(done.stdout)
    except ValueError:
        return {"ok": False, "took": took, "rc": done.returncode, "error": (done.stdout + done.stderr)[-300:]}
    if body.get("ok") is False:
        error = body.get("error", {})
        return {"ok": False, "took": took, "code": error.get("code"), "support": error.get("support_reference"), "message": error.get("message", "")[:200]}
    return {"ok": True, "took": took, "id": body.get("id"), "state": body.get("state"), "edge_state": body.get("edge_state")}


def pages(state=None):
    """Every page of `pb preview list` (200 each), following the printed cursor; yields (rows, server total)."""
    cursor = None
    while True:
        args = ["preview", "list", "--limit", "200"] + (["--state", state] if state else []) + (["--cursor", cursor] if cursor else [])
        done = run(args, timeout=300)
        lines = (done.stdout + done.stderr).splitlines()  # the paging footer goes to stderr
        rows = [line.split("\t") for line in lines if "\t" in line]
        total = next((int(line.split(" of ")[1].split()[0]) for line in lines if line.startswith("Showing ")), len(rows))
        yield rows, total
        cursor = next((line.split("--cursor ")[1].split()[0] for line in lines if line.startswith("Next page:")), None)
        if not cursor:
            return


def live_ids():
    return [row[0] for rows, _ in pages() for row in rows if row[3] not in ("stopped", "expired")]


def total():
    """The server's count of previews in one request (`pb preview list --limit 1` prints 'Showing 1 of N')."""
    done = run(["preview", "list", "--limit", "1"])
    lines = (done.stdout + done.stderr).splitlines()
    return next((int(line.split(" of ")[1].split()[0]) for line in lines if line.startswith("Showing ")), None)


def main():
    mode = sys.argv[1]
    if mode == "create":
        n, parallel, port = int(sys.argv[2]), int(sys.argv[3]), int(sys.argv[4])
        started = time.time()
        results = []
        with concurrent.futures.ThreadPoolExecutor(parallel) as pool:
            for result in pool.map(lambda _: create(port), range(n)):
                results.append(result)
                print(json.dumps(result), flush=True)
        ok = [r for r in results if r["ok"]]
        codes = {}
        for r in results:
            if not r["ok"]:
                codes[r.get("code") or "unparsed"] = codes.get(r.get("code") or "unparsed", 0) + 1
        print(json.dumps({"summary": True, "requested": n, "created": len(ok), "failed": n - len(ok), "failed_by_code": codes, "seconds": round(time.time() - started, 1),
                          "per_second": round(len(ok) / max(time.time() - started, 1), 2)}))
    elif mode == "total":
        print(total())
    elif mode == "delete":
        parallel = int(sys.argv[2])
        ids = live_ids()
        with concurrent.futures.ThreadPoolExecutor(parallel) as pool:
            failed = [i for i, done in zip(ids, pool.map(lambda i: run(["preview", "delete", i]).returncode, ids)) if done]
        print(json.dumps({"deleted": len(ids) - len(failed), "failed": failed[:5], "live_after": len(live_ids())}))
    else:
        sys.exit(__doc__)


main()
