#!/usr/bin/env python3
"""Status page probe (vantage point): real user-path checks, signed and posted to the status Worker.

A vantage point checks its own internet first and again after any failure; if that fails, it sends a heartbeat with no results,
so a broken probe host can never make a service look down. Config (JSON, path in STATUS_PROBE_CONFIG; never committed):
  url        the Worker's /api/probe endpoint        vantage   this host's name in status/config/components.json
  key_file   file holding the vantage's signing key  net_urls  two or more URLs that must answer for "my internet works"
  checks     [{unit, kind: http|relay|tuf|tcp, url|host+port, expect?, redirect? (a 3xx is healthy), resolve_all? (every DNS address),
              pin? (sha256//<base64 public key hash> for one check)}]
  relay_set_cmd  a shell command that prints this host's canary machine's relay file (the signed candidate set its daemon keeps); relay checks
             trust the key the SIGNED set names for that relay, the way clients do, so a certificate rotation follows the set, never this config  steps     {probe.sh step name: unit} for canary steps run on this host
Used by canary.py (oracle, with its steps) and status_run.py (anvit and staging).
"""
import base64, hashlib, hmac, json, os, re, socket, ssl, subprocess, sys, time, urllib.request
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone

CHECK_TIMEOUT = 10


def curl(url, resolve=None, pin=None, insecure=False):
    """(http_code, seconds, body) or raises RuntimeError with the curl error."""
    cmd = ["curl", "-sS", "--max-time", str(CHECK_TIMEOUT), "-o", "-", "-w", "\n%{http_code} %{time_total}"]
    if resolve:
        cmd += ["--resolve", resolve]
    if insecure or pin:  # a relay's certificate is private (no CA): its identity is the pinned key when the signed set names one, else only validity is judged
        cmd.append("-k")
    if pin:
        cmd += ["--pinnedpubkey", pin]  # checked even with -k
    done = subprocess.run(cmd + [url], capture_output=True, timeout=CHECK_TIMEOUT + 5)
    if done.returncode != 0:
        raise RuntimeError(done.stderr.decode(errors="replace").strip()[-150:] or f"curl exit {done.returncode}")
    body, _, tail = done.stdout.rpartition(b"\n")
    code, seconds = tail.decode().split()
    return int(code), float(seconds), body.decode(errors="replace")


def relay_pins(text, now=None):
    """{(host, port): 'sha256//<base64>'} from a relay file (the daemon's copy of the signed candidate set). The set is already
    verified by the daemon that wrote it; an expired or unreadable set gives no identities, never a guess."""
    try:
        token = json.loads(text)["candidate_set"]
        payload = token.split(".")[1]
        claims = json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))
        if claims["exp"] <= (now if now is not None else time.time()):
            return {}
        pins = {}
        for node in claims["nodes"]:
            spki = node.get("tls_spki_sha256")
            if spki and node.get("endpoint_host") and node.get("endpoint_tcp_port"):
                raw = base64.urlsafe_b64decode(spki + "=" * (-len(spki) % 4))
                pins[(node["endpoint_host"], int(node["endpoint_tcp_port"]))] = "sha256//" + base64.b64encode(raw).decode()
        return pins
    except (ValueError, KeyError, TypeError, IndexError):
        return {}


def load_relay_pins(cfg):
    cmd = cfg.get("relay_set_cmd")
    if not cmd:
        return {}
    try:
        done = subprocess.run(cmd, shell=True, capture_output=True, text=True, timeout=20)
        return relay_pins(done.stdout) if done.returncode == 0 else {}
    except (OSError, subprocess.TimeoutExpired):
        return {}


def with_identities(checks, pins):
    """Relay checks get the key the signed set names for their host and port, when it names one."""
    out = []
    for c in checks:
        if c.get("kind") == "relay" and "pin" not in c:
            hostport = c["url"].split("://", 1)[1].split("/", 1)[0]
            host, _, port = hostport.rpartition(":")
            pin = pins.get((host, int(port))) if port.isdigit() else None
            c = {**c, "pin": pin} if pin else c
        out.append(c)
    return out


def cert_valid_now(host, port, now=None):
    """The relay's leaf certificate is inside its validity period (clients check this too: nodetls verifies the leaf against itself)."""
    context = ssl.create_default_context()
    context.check_hostname, context.verify_mode = False, ssl.CERT_NONE
    with socket.create_connection((host, int(port)), timeout=5) as raw, context.wrap_socket(raw) as tls:
        pem = ssl.DER_cert_to_PEM_cert(tls.getpeercert(binary_form=True))
    out = subprocess.run(["openssl", "x509", "-noout", "-startdate", "-enddate"], input=pem, capture_output=True, text=True, timeout=10).stdout
    when = {k: ssl.cert_time_to_seconds(v) for k, v in re.findall(r"(notBefore|notAfter)=(.+)", out)}
    t = now if now is not None else time.time()
    return when["notBefore"] <= t <= when["notAfter"]


def one(check):
    """One check -> (ok, ms, error)."""
    kind, start = check["kind"], time.monotonic()
    try:
        if kind == "tcp":
            socket.create_connection((check["host"], int(check["port"])), timeout=5).close()
            return True, round((time.monotonic() - start) * 1000), ""
        code, seconds, body = curl(check["url"], check.get("resolve"), check.get("pin"), kind == "relay")
        ms = round(seconds * 1000)
        if kind == "relay":  # a relay's TLS port answers an unauthenticated GET with 401; 503 means full or draining
            hostport = check["url"].split("://", 1)[1].split("/", 1)[0]
            host, _, port = hostport.rpartition(":")
            if not cert_valid_now(host, port):
                return False, ms, "relay certificate is expired or not yet valid"
            ok = code == 401 and "relay authorization required" in body
            return ok, ms, "" if ok else f"relay answered {code}"
        if kind == "tuf":
            stamp = re.sub(r"(\.\d{6})\d+", r"\1", json.loads(body)["signed"]["expires"].replace("Z", "+00:00"))  # older Pythons read at most 6 fraction digits
            expires = datetime.fromisoformat(stamp)
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
        except Exception:  # any failure, however odd its shape, means "not reachable"
            return False
    with ThreadPoolExecutor(max_workers=4) as pool:  # two answers, so one captive-portal or one dead URL cannot decide it
        return len(net_urls) >= 2 and sum(pool.map(reach, net_urls)) >= 2


def step_results(steps, mapping):
    """probe.sh steps (JSON lines already parsed) -> results for the units they stand for."""
    out = [{"unit": mapping[s["step"]], "ok": s["ok"], "ms": int(s.get("seconds", 0)) * 1000, "error": s.get("error", "")[:150]}
           for s in steps if s["step"] in mapping]
    broken = next((s for s in steps if s["step"] in ("probe", "account") and not s["ok"]), None)
    if broken:  # the runner itself failed or timed out: no step it should have run can be vouched for
        out += [{"unit": u, "ok": False, "ms": 0, "error": broken.get("error", "probe did not finish")[:150]} for u in set(mapping.values()) if not any(r["unit"] == u for r in out)]
    return out


def body_and_signature(cfg, results, net_ok, now=None):
    ts = int((now if now is not None else time.time()) * 1000)
    body = json.dumps({"vantage": cfg["vantage"], "ts": ts, "net_ok": net_ok, "build": "probe-1",
                       "results": results if net_ok else []}, separators=(",", ":"))
    key = open(cfg["key_file"]).read().strip()
    return body, hmac.new(key.encode(), f"{cfg['vantage']}\n{ts}\n{body}".encode(), hashlib.sha256).hexdigest()


def post(cfg, results, net_ok):
    try:
        body, sig = body_and_signature(cfg, results, net_ok)
    except (OSError, KeyError) as error:
        print(f"status key file unreadable: {error}", file=sys.stderr)
        return False
    request = urllib.request.Request(cfg["url"], data=body.encode(), method="POST",
                                     headers={"content-type": "application/json", "x-status-signature": sig,
                                              "user-agent": "paperboat-status-probe/1"})  # Cloudflare refuses the default Python agent (error 1010)
    for attempt in range(2):
        try:
            with urllib.request.urlopen(request, timeout=10) as res:
                return res.status == 200
        except OSError:
            time.sleep(1 + attempt)
    return False


def report(cfg, extra_results=()):
    """Run this host's checks and post them; returns what was posted (None when the host's internet is down)."""
    try:
        net_ok = internet_ok(cfg["net_urls"])
        results = run_checks(with_identities(cfg.get("checks", []), load_relay_pins(cfg))) + list(extra_results) if net_ok else []
        if net_ok and any(not r["ok"] for r in results):  # a drop during the run must not read as an outage
            net_ok = internet_ok(cfg["net_urls"])
    except Exception as error:  # a bug in this host's own checking: say so with a heartbeat that vouches for nothing
        print(f"status probe failed: {error}", file=sys.stderr)
        net_ok, results = False, []
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
