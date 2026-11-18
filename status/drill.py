#!/usr/bin/env python3
"""Drill: stop a relay, an edge and the API of a staging stack one at a time and record how the status page reacts.

For each service: stop it, wait for its component to go red (major), start it again, wait for green. Every poll of the page's
JSON is a line in the results file; the verdict per service says how long red and green took and whether any other component moved.
usage: drill.py STATUS_URL SSH_HOST OUT.md [component=systemd-user-unit ...]
"""
import json, subprocess, sys, time, urllib.request
from datetime import datetime, timezone

RED_LIMIT, GREEN_LIMIT, POLL = 420, 900, 5


def states(url):
    with urllib.request.urlopen(url + "/api/status", timeout=10) as r:
        d = json.load(r)
    return d["delayed"], {c["id"]: c["state"] for c in d["public"]["components"]}


def ssh(host, cmd):
    subprocess.run(["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", host, cmd], check=True, timeout=60)


def wait(url, log, want_component, want_state, limit, others_ok):
    start, moved = time.monotonic(), set()
    while time.monotonic() - start < limit:
        delayed, s = states(url)
        log.append({"t": datetime.now(timezone.utc).isoformat(timespec="seconds"), "delayed": delayed, **s})
        moved |= {c for c, v in s.items() if c != want_component and v != others_ok}
        if s.get(want_component) == want_state and not delayed:
            return round(time.monotonic() - start), sorted(moved)
        time.sleep(POLL)
    return None, sorted(moved)


def main():
    url, host, out, *pairs = sys.argv[1:]
    targets = [p.split("=", 1) for p in pairs]
    log, rows = [], []
    ok = True
    for component, unit in targets:
        _, s = states(url)
        assert all(v == "operational" for v in s.values()), f"not green before {component}: {s}"
        ssh(host, f"systemctl --user stop {unit}")
        red, moved_red = wait(url, log, component, "major", RED_LIMIT, "operational")
        ssh(host, f"systemctl --user start {unit}")
        green, moved_green = wait(url, log, component, "operational", GREEN_LIMIT, "operational")
        others = sorted(set(moved_red) | set(moved_green))
        good = red is not None and green is not None and not others
        ok &= good
        rows.append((component, unit, red, green, others, good))
        # everything must be green again before the next service is stopped
        wait(url, log, component, "operational", GREEN_LIMIT, "operational")
    with open(out, "w") as f:
        f.write(f"# Status page drill {datetime.now(timezone.utc).strftime('%Y-%m-%d %H:%M')} UTC\n\n")
        f.write("| Component | Service stopped | Red after (s) | Green after start (s) | Other components moved | Result |\n|---|---|---|---|---|---|\n")
        for c, u, r, g, o, good in rows:
            f.write(f"| {c} | {u} | {r} | {g} | {', '.join(o) or 'none'} | {'PASS' if good else 'FAIL'} |\n")
        f.write("\n## Samples\n\n```\n" + "\n".join(json.dumps(x) for x in log) + "\n```\n")
    print("PASS" if ok else "FAIL")
    sys.exit(0 if ok else 1)


try:
    main()
finally:  # never leave a staging service stopped
    for pair in sys.argv[4:]:
        subprocess.run(["ssh", "-o", "BatchMode=yes", sys.argv[2], f"systemctl --user start {pair.split('=', 1)[1]}"], timeout=60)
