#!/usr/bin/env python3
"""Launch progress dashboard: renders CHECKLIST.md from origin/main as one HTML page.

Usage: tools/progress.py [--listen HOST:PORT]   (default 127.0.0.1:8090)
Shows independently verified completion for Linux, macOS and Windows.
"""
import argparse, html, re, subprocess, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
MARKS = ("[x]", "[ ]", "[!]", "[~]", "n/a", "—")
FETCH_EVERY = 60
_last_fetch = 0.0


def checklist() -> str:
    global _last_fetch
    if time.time() - _last_fetch > FETCH_EVERY:
        subprocess.run(["git", "-C", str(REPO), "fetch", "-q", "origin", "main"], timeout=30)
        _last_fetch = time.time()
    return subprocess.run(["git", "-C", str(REPO), "show", "origin/main:CHECKLIST.md"],
                          capture_output=True, text=True, timeout=30, check=True).stdout


def parse(text):
    sections, current = [], None
    for line in text.splitlines():
        if m := re.match(r"^## ((?:F|S)\d+)\. (.+)", line):
            current = {"id": m[1], "name": m[2], "feature": [], "command": []}
            sections.append(current)
            continue
        m = re.match(r"^\|\s*((F|S)\d+-\d+|C\d+)\s*\|", line)
        if not (m and current):
            continue
        cells = [c.strip() for c in line.strip().strip("|").split("|")]
        start = 3 if cells[0].startswith("C") else 2
        marks = cells[start:start + 3]
        if len(marks) != 3 or any(mark not in MARKS for mark in marks):
            continue
        row = {"id": cells[0], "text": cells[1] if cells[0].startswith(("F", "S")) else f"{cells[1]} — {cells[2]}",
               "platforms": marks[:3], "evidence": cells[-1]}
        current["command" if cells[0].startswith("C") else "feature"].append(row)
    return sections


def bar(done, total, blocked=0):
    if not total:
        return '<div class="bar"></div>'
    d, b = 100 * done / total, 100 * blocked / total
    return (f'<div class="bar"><i class="d" style="width:{d:.1f}%"></i>'
            f'<i class="b" style="width:{b:.1f}%"></i></div>')


def count(rows, platform):
    marks = [r["platforms"][platform] for r in rows if len(r["platforms"]) > platform]
    return marks.count("[x]"), marks.count("[!]"), sum(m in ("[x]", "[ ]", "[!]", "[~]") for m in marks)


def render_platform(sections, platform, name):
    feats = [r for s in sections if s["id"].startswith("F") for r in s["feature"]]
    cmds = [r for s in sections for r in s["command"]]
    out = []
    for label, rows in (("Features", feats), ("Commands", cmds)):
        d, b, t = count(rows, platform)
        out.append(f'<div class="card"><div class="big">{d}<span>/{t}</span></div>'
                   f'<div>{label} verified on {name}</div>{bar(d, t, b)}'
                   f'<div class="sub">{100*d//max(t,1)}% · {b} blocked</div></div>')
    head = f'<h2>{name}</h2><div class="cards">{"".join(out)}</div>'

    table = ['<table><tr><th></th><th>Area</th><th>Features</th><th></th><th>Commands</th><th></th></tr>']
    for s in sections:
        if s["id"].startswith("S"):
            continue
        fd, fb, ft = count(s["feature"], platform)
        cd, cb, ct = count(s["command"], platform)
        table.append(f'<tr><td class="id">{s["id"]}</td><td>{html.escape(s["name"])}</td>'
                     f'<td class="n">{fd}/{ft}</td><td class="w">{bar(fd, ft, fb)}</td>'
                     f'<td class="n">{cd}/{ct if ct else "–"}</td><td class="w">{bar(cd, ct, cb)}</td></tr>')
    table.append("</table>")

    blocked = [(s, r) for s in sections for r in s["feature"] + s["command"] if len(r["platforms"]) > platform and r["platforms"][platform] == "[!]"]
    items = "".join(
        f'<details><summary><b>{r["id"]}</b> {html.escape(r["text"][:140])}</summary>'
        f'<p>{html.escape(r["evidence"][:600])}</p></details>' for _, r in blocked)
    return (head + "".join(table)
            + f'<h3>Blocked on {name} ({len(blocked)})</h3>' + items)


def render(sections):
    platforms = ((0, "Linux — current focus"), (1, "macOS — next"), (2, "Windows — after macOS"))
    content = "".join(render_platform(sections, index, name) for index, name in platforms)
    infra = [r for s in sections if s["id"].startswith("S") for r in s["feature"]]
    done, blocked, total = count(infra, 0)
    content += (f'<h2>Shared setup &amp; release</h2><p>{done}/{total} verified · {blocked} blocked</p>'
                + bar(done, total, blocked))
    return PAGE.format(content=content, stamp=time.strftime("%Y-%m-%d %H:%M:%S %Z"))


PAGE = """<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="60">
<title>Paperboat launch progress</title><style>
body{{font:15px/1.4 -apple-system,system-ui,sans-serif;margin:32px auto;max-width:1000px;color:#111;padding:0 16px}}
h1{{font-size:22px;margin:0 0 4px}} .muted{{color:#777;font-size:13px}}
.cards{{display:grid;grid-template-columns:repeat(2,1fr);gap:16px;margin:24px 0}}
.card{{border:1px solid #e5e5e5;border-radius:12px;padding:16px}}
.big{{font-size:40px;font-weight:600}} .big span{{font-size:20px;color:#999}} .sub{{color:#777;font-size:13px;margin-top:6px}}
.bar{{height:8px;background:#eee;border-radius:4px;overflow:hidden;display:flex;margin-top:8px}}
.bar i{{display:block;height:100%}} .d{{background:#1f7a3a}} .b{{background:#d9822b}}
table{{width:100%;border-collapse:collapse}} td,th{{padding:7px 6px;border-bottom:1px solid #f0f0f0;text-align:left}}
th{{font-size:12px;color:#888;font-weight:500}} .id{{color:#888;width:40px}} .n{{width:60px;text-align:right;font-variant-numeric:tabular-nums}}
.w{{width:150px}} .w .bar{{margin:0}} details{{border-bottom:1px solid #f0f0f0;padding:6px 0}}
summary{{cursor:pointer}} details p{{color:#555;font-size:13px;white-space:pre-wrap}}
.legend i{{display:inline-block;width:10px;height:10px;border-radius:2px;margin:0 4px 0 12px}}
</style>
<h1>Paperboat launch progress</h1>
<div class="muted">From CHECKLIST.md on origin/main · updated {stamp} · refreshes every minute
<span class="legend"><i class="d" style="background:#1f7a3a"></i>verified on this platform<i style="background:#d9822b"></i>blocked</span></div>
<p class="muted">Each platform counts its own passing checks. Not-applicable cells are excluded. Order: Linux, then macOS, then Windows.</p>
{content}
"""


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        try:
            body, status = render(parse(checklist())).encode(), 200
        except Exception as error:  # show the failing step instead of a blank page
            body, status = f"progress: reading CHECKLIST.md failed: {error}".encode(), 500
        self.send_response(status)
        self.send_header("content-type", "text/html; charset=utf-8")
        self.send_header("cache-control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--listen", default="127.0.0.1:8090")
    host, port = parser.parse_args().listen.rsplit(":", 1)
    ThreadingHTTPServer((host, int(port)), Handler).serve_forever()
