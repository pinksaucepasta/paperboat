#!/usr/bin/env python3
"""Status page probe (vantage point): real user-path checks, signed and posted to the status Worker.

A vantage point checks its own internet first and again after any failure; if that fails, it sends a heartbeat with no results,
so a broken probe host can never make a service look down. Config (JSON, path in STATUS_PROBE_CONFIG; never committed):
  url        the Worker's /api/probe endpoint        vantage   this host's name in status/config/components.json
  key_file   file holding the vantage's signing key  net_urls  two or more URLs that must answer for "my internet works"
  checks     [{unit, kind: http|relay|tuf|tcp, url|host+port, expect?, redirect? (a 3xx is healthy), resolve_all? (every DNS address)}]  steps     {probe.sh step name: unit} for canary steps run on this host
Used by canary.py (oracle, with its steps) and status_run.py (anvit and staging).
"""
import hashlib, hmac, json, os, socket, subprocess, sys, time, urllib.request
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone

CHECK_TIMEOUT = 10


def curl(url, resolve=None, insecure=False):
    """(http_code, seconds, body) or raises RuntimeError with the curl error."""
    cmd = ["curl", "-sS", "--max-time", str(CHECK_TIMEOUT), "-o", "-", "-w", "\n%{http_code} %{time_total}"]
    if resolve:
        cmd += ["--resolve", resolve]
    if insecure:
        cmd.append("-k")
    done = subprocess.run(cmd + [url], capture_output=True, timeout=CHECK_TIMEOUT + 5)
    if done.returncode != 0:
        raise RuntimeError(done.stderr.decode(errors="replace").strip()[-150:] or f"curl exit {done.returncode}")
    body, _, tail = done.stdout.rpartition(b"\n")
    code, seconds = tail.decode().split()
    return int(code), float(seconds), body.decode(errors="replace")


def one(check):
    """One check -> (ok, ms, error)."""
    kind, start = check["kind"], time.monotonic()
    try:
        if kind == "tcp":
            socket.create_connection((check["host"], int(check["port"])), timeout=5).close()
            return True, round((time.monotonic() - start) * 1000), ""
        code, seconds, body = curl(check["url"], check.get("resolve"), kind == "relay")
        ms = round(seconds * 1000)
        if kind == "relay":  # a relay's TLS port answers an unauthenticated GET with 401; 503 means full or draining
            ok = code == 401 and "relay authorization required" in body
            return ok, ms, "" if ok else f"relay answered {code}"
        if kind == "tuf":
            expires = datetime.fromisoformat(json.loads(body)["signed"]["expires"].replace("Z", "+00:00"))
            expires = expires if expires.tzinfo else expires.replace(tzinfo=timezone.utc)
            ok = code == 200 and expires > datetime.now(timezone.utc)
            return ok, ms, "" if ok else "release metadata expired or not served"
        # 2xx is a pass; a 3xx only for a check that says a redirect is the healthy answer (curl does not follow it)
        good = 200 <= code < (400 if check.get("redirect") else 300)
        ok = good and check.get("expect", "") in body
        return ok, ms, "" if ok else f"status {code}" if not good else "unexpected content"
    except (RuntimeError, OSError, ValueError, KeyError, TypeError, subprocess.TimeoutExpired) as error:
        return False, round((time.monotonic() - start) * 1000), str(error)[:150]


def run_check(check):
    if check.get("resolve_all"):  # every address in DNS must answer: one dead edge must not hide behind a healthy one
        host = check["url"].split("://", 1)[1].split("/", 1)[0].split(":")[0]
        try:
            ips = sorted({i[4][0] for i in socket.getaddrinfo(host, 443, socket.AF_INET)})
        except OSError as error:
            return False, 0, f"dns for {host}: {error}"
        runs = [one({**check, "resolve": f"{host}:443:{ip}"}) for ip in ips] or [(False, 0, f"no address for {host}")]
        bad = next((r for r in runs if not r[0]), None)
        return bad or (True, max(r[1] for r in runs), "")
    return one(check)


def run_checks(checks):
    with ThreadPoolExecutor(max_workers=8) as pool:
        done = list(pool.map(run_check, checks))
    return [{"unit": c["unit"], "ok": ok, "ms": ms, "error": err} for c, (ok, ms, err) in zip(checks, done)]


def internet_ok(net_urls):
    def reach(url):
        try:
            return 200 <= curl(url)[0] < 400
        except (RuntimeError, OSError, subprocess.TimeoutExpired):
            return False
    with ThreadPoolExecutor(max_workers=4) as pool:  # two answers, so one captive-portal or one dead URL cannot decide it
        return len(net_urls) >= 2 and sum(pool.map(reach, net_urls)) >= 2


def step_results(steps, mapping):
    """probe.sh steps (JSON lines already parsed) -> results for the units they stand for."""
    return [{"unit": mapping[s["step"]], "ok": s["ok"], "ms": int(s.get("seconds", 0)) * 1000, "error": s.get("error", "")[:150]}
            for s in steps if s["step"] in mapping]


def body_and_signature(cfg, results, net_ok, now=None):
    ts = int((now if now is not None else time.time()) * 1000)
    body = json.dumps({"vantage": cfg["vantage"], "ts": ts, "net_ok": net_ok, "build": "probe-1",
                       "results": results if net_ok else []}, separators=(",", ":"))
    key = open(cfg["key_file"]).read().strip()
    return body, hmac.new(key.encode(), f"{cfg['vantage']}\n{ts}\n{body}".encode(), hashlib.sha256).hexdigest()


def post(cfg, results, net_ok):
    try:
        body, sig = body_and_signature(cfg, results, net_ok)
    except OSError as error:
        print(f"status key file unreadable: {error}", file=sys.stderr)
        return False
    request = urllib.request.Request(cfg["url"], data=body.encode(), method="POST",
                                     headers={"content-type": "application/json", "x-status-signature": sig})
    for attempt in range(2):
        try:
            with urllib.request.urlopen(request, timeout=10) as res:
                return res.status == 200
        except OSError:
            time.sleep(1 + attempt)
    return False


def report(cfg, extra_results=()):
    """Run this host's checks and post them; returns what was posted (None when the host's internet is down)."""
    net_ok = internet_ok(cfg["net_urls"])
    results = run_checks(cfg.get("checks", [])) + list(extra_results) if net_ok else []
    if net_ok and any(not r["ok"] for r in results):  # a drop during the run must not read as an outage
        net_ok = internet_ok(cfg["net_urls"])
    if not post(cfg, results if net_ok else [], net_ok):  # a rotated key or a down Worker must not pass silently
        print(f"status report not accepted by {cfg['url']}", file=sys.stderr)
    return results if net_ok else None


def load_config(path=None):
    path = path or os.environ.get("STATUS_PROBE_CONFIG")
    return json.load(open(path)) if path else None


if __name__ == "__main__":
    config = load_config()
    if not config:
        sys.exit("STATUS_PROBE_CONFIG is not set")
    print(json.dumps(report(config)))
