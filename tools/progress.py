#!/usr/bin/env python3
"""Launch progress dashboard: renders CHECKLIST.md from origin/main as one HTML page.

Usage: tools/progress.py [--listen HOST:PORT]   (default 127.0.0.1:8090)
Shows independently verified completion for Linux, macOS and Windows.
"""
import argparse, collections, html, json, re, subprocess, threading, time
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
MARKS = ("[x]", "[ ]", "[!]", "[~]", "n/a", "—")
FETCH_EVERY = 60
CANARY = Path.home() / ".local/state/paperboat-canary"  # written by tools/probe/canary.py
IST = timezone(timedelta(hours=5, minutes=30))
_page = "progress: first read of CHECKLIST.md still running, reload in a minute"


def checklist() -> str:
    subprocess.run(["git", "-C", str(REPO), "fetch", "-q", "origin", "main"], timeout=120)
    return subprocess.run(["git", "-C", str(REPO), "show", "origin/main:CHECKLIST.md"],
                          capture_output=True, text=True, timeout=60, check=True).stdout


def refresh_forever():
    """Rebuild the page in the background so a slow git never blocks a page load."""
    global _page
    while True:
        try:
            _page = render(parse(checklist()))
        except Exception as error:  # keep serving the last good page; say why it is stale
            _page = _page.replace("updated ", f"refresh failed ({error}); last updated ", 1)
        time.sleep(FETCH_EVERY)


def parse(text):
    sections, current = [], None
    for line in text.splitlines():
        if m := re.match(r"^## ((?:F|S)\d+|X|P|R|W)\. (.+)", line):
            current = {"id": m[1], "name": m[2], "feature": [], "command": []}
            sections.append(current)
            continue
        m = re.match(r"^\|\s*((F|S|X)\d*-\d+|P[RQ]-\d+|[RW]-\d+|C\d+)\s*\|", line)
        if not (m and current):
            continue
        cells = [c.strip() for c in line.strip().strip("|").split("|")]
        start = 3 if cells[0].startswith("C") else 2
        marks = cells[start:start + 3]
        if len(marks) != 3 or any(mark not in MARKS for mark in marks):
            continue
        row = {"id": cells[0], "text": cells[1] if cells[0].startswith(("F", "S", "X", "P", "R", "W")) else f"{cells[1]} — {cells[2]}",
               "platforms": marks[:3], "evidence": cells[-1]}
        current["command" if cells[0].startswith("C") else "feature"].append(row)
    return sections


def canary():
    """The production canary's status and its last 60 runs; read on every request, never cached."""
    try:
        status = json.loads((CANARY / "status.json").read_text())
    except (OSError, ValueError):
        status = None
    try:
        with open(CANARY / "history.jsonl") as lines:
            history = [json.loads(line) for line in collections.deque(lines, 60)]
    except (OSError, ValueError):
        history = []
    return {"status": status, "history": history}


def ist(stamp):
    try:
        return datetime.strptime(stamp, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc).astimezone(IST)
    except (TypeError, ValueError):
        return None


def canary_line(state, now=None):
    status, now = state["status"], now or datetime.now(IST)
    run = (status or {}).get("last") or {}
    started = ist(run.get("started"))
    if not status or not started:
        text, ok = "no result yet", False
    elif (now - started).total_seconds() > 300:
        text, ok = f"STALE: last run {started:%H:%M} IST (the prober on oracle is not running)", False
    elif status.get("red"):
        failed = ", ".join(s["step"] for s in run.get("steps", []) if not s.get("ok"))
        since = ist(status.get("red_since")) or started
        text, ok = f"RED since {since:%H:%M} IST: {failed}", False
    else:
        steps = run.get("steps", [])
        text, ok = f"green: {len(steps)}/{len(steps)} checks, last run {started:%H:%M} IST", True
    cells = "".join(
        f'<i class="{"d" if r.get("ok") else "r"}" title="{html.escape(r.get("started", ""))}'
        f'{"" if r.get("ok") else ": " + html.escape(", ".join(s["step"] for s in r.get("steps", []) if not s.get("ok")))}"></i>'
        for r in state["history"])
    return (f'<div class="card canary {"" if ok else "red"}"><div class="row"><span>Production canary '
            f'(exec, ssh, send, preview, tunnel between two machines, every minute)</span></div>'
            f'<div class="row"><b>{html.escape(text)}</b></div><div class="strip">{cells}</div></div>')


def bar(done, total, blocked=0):
    if not total:
        return '<div class="bar"></div>'
    d, b = 100 * done / total, 100 * blocked / total
    return (f'<div class="bar"><i class="d" style="width:{d:.1f}%"></i>'
            f'<i class="b" style="width:{b:.1f}%"></i></div>')


def count(rows, platform):
    marks = [r["platforms"][platform] for r in rows if len(r["platforms"]) > platform]
    return marks.count("[x]"), marks.count("[!]"), sum(m in ("[x]", "[ ]", "[!]", "[~]") for m in marks)


def count_rows(rows):
    """Single-total cards: a row counts once, done only when every platform it applies to is done."""
    done = blocked = total = 0
    for r in rows:
        marks = [m for m in r["platforms"] if m in ("[x]", "[ ]", "[!]", "[~]")]
        if not marks:
            continue
        total += 1
        if all(m == "[x]" for m in marks):
            done += 1
        elif "[!]" in marks:
            blocked += 1
    return done, blocked, total


def render(sections):
    # Product features count per platform; reliability/scale, the SDK, release safety and the
    # website/console get their own cards (user decision 2026-10-26).
    groups = {"reliability": ("F21", "P"), "sdk": ("F24",), "release": ("R",), "website": ("W",)}
    def rows_of(ids):
        return [r for s in sections if s["id"] in ids for r in s["feature"]]
    product = [r for s in sections if s["id"].startswith("F")
               and s["id"] not in groups["reliability"] + groups["sdk"] for r in s["feature"]]
    # A row that is n/a on macOS and Windows is server or console work, not an OS feature: it gets its
    # own card so every OS card counts the same features (user 2026-11-05).
    neutral = lambda r: all(m in ("n/a", "—") for m in r["platforms"][1:3])
    feats = [r for r in product if not neutral(r)]
    server = [r for r in product if neutral(r)]
    cmds = [r for s in sections for r in s["command"]]
    cards = []
    for index, name in ((0, "Linux"), (1, "macOS"), (2, "Windows")):
        lines = []
        for label, rows in (("Features", feats), ("Commands", cmds)):
            d, b, t = count(rows, index)
            lines.append(f'<div class="row"><span>{label}</span><b>{d}/{t}</b>'
                         f'<em>{100*d//max(t,1)}%</em></div>{bar(d, t, b)}')
        cards.append(f'<div class="card"><h2>{name}</h2>{"".join(lines)}</div>')
    if server:
        d, b, t = count_rows(server)
        cards.append(f'<div class="card"><h2>Server and console features</h2><div class="row"><span>Done</span>'
                     f'<b>{d}/{t}</b><em>{100*d//max(t,1)}%</em></div>{bar(d, t, b)}</div>')
    for title, ids in (("Reliability, scale and efficiency", groups["reliability"]),
                       ("SDK and agent tools", groups["sdk"]),
                       ("Release safety", groups["release"]),
                       ("Website and console", groups["website"])):
        rows = rows_of(ids)
        if rows:
            d, b, t = count_rows(rows)
            cards.append(f'<div class="card"><h2>{title}</h2><div class="row"><span>Done</span>'
                         f'<b>{d}/{t}</b><em>{100*d//max(t,1)}%</em></div>{bar(d, t, b)}</div>')
    cross = [r for s in sections if s["id"] == "X" for r in s["feature"]]
    if not cross:
        cards.append('<div class="card"><h2>Cross-platform</h2><div class="row"><span>'
                     'Rows are being written (CHECKLIST section X)</span></div></div>')
    else:
        lines = []
        for index, pair in ((0, "Linux ↔ macOS"), (1, "Linux ↔ Windows"), (2, "macOS ↔ Windows")):
            d, b, t = count(cross, index)
            lines.append(f'<div class="row"><span>{pair}</span><b>{d}/{t}</b>'
                         f'<em>{100*d//max(t,1)}%</em></div>{bar(d, t, b)}')
        cards.append(f'<div class="card"><h2>Cross-platform</h2>{"".join(lines)}</div>')
    return PAGE.format(content="".join(cards), stamp=time.strftime("%H:%M %Z"))


PAGE = """<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="60">
<title>Paperboat launch progress</title><style>
body{{font:16px/1.4 -apple-system,system-ui,sans-serif;margin:40px auto;max-width:640px;color:#111;padding:0 16px}}
h1{{font-size:22px;margin:0}} .muted{{color:#888;font-size:13px;margin-bottom:24px}}
.card{{border:1px solid #e5e5e5;border-radius:12px;padding:16px 20px;margin-bottom:16px}}
h2{{font-size:18px;margin:0 0 8px}} .row{{display:flex;gap:12px;margin-top:10px}}
.row span{{flex:1}} .row em{{color:#888;font-style:normal;width:44px;text-align:right}}
.bar{{height:10px;background:#eee;border-radius:5px;overflow:hidden;display:flex;margin-top:6px}}
.bar i{{display:block;height:100%}} .d{{background:#1f7a3a}} .b{{background:#d9822b}} .r{{background:#c0392b}}
.canary.red{{border-color:#c0392b}} .canary.red b{{color:#c0392b}} .strip{{display:flex;gap:2px;margin-top:8px}}
.strip i{{display:block;width:6px;height:14px;border-radius:1px}}
</style>
<h1>Paperboat launch progress</h1>
<div class="muted">Verified rows from CHECKLIST.md on main · green done, orange blocked · updated {stamp}</div>
<!--canary-->{content}
"""


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.split("?")[0] == "/canary.json":
            body, kind = json.dumps(canary()).encode(), "application/json"
        else:
            body, kind = _page.replace("<!--canary-->", canary_line(canary()), 1).encode(), "text/html; charset=utf-8"
        self.send_response(200)
        self.send_header("content-type", kind)
        self.send_header("cache-control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--listen", default="127.0.0.1:8090")
    host, port = parser.parse_args().listen.rsplit(":", 1)
    threading.Thread(target=refresh_forever, daemon=True).start()
    ThreadingHTTPServer((host, int(port)), Handler).serve_forever()
