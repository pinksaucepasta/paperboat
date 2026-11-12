#!/usr/bin/env python3
"""F8-14: edge flood protection against a real flood, from two real source addresses.

A disposable enrolled machine (--machine-cmd prefix, or --windows) runs origin_proxy_matrix/app.py behind a tunnel. Observer A is
this host, observer B is --observer-b-cmd (e.g. `ssh ubuntu@anvit`), so the edge sees two client addresses. B floods the tunnel
with persistent HTTPS connections; while it floods A keeps visiting. Checks:
  - B is refused 429 `edge_rate_limited` with Retry-After once it is over its per-address limit, and gets some answers first (the burst);
  - A is served 200 every time during B's flood (one noisy address is limited, the others are not);
  - the refusal is a page for a browser (Accept: text/html) and problem+json for an API client (Accept: application/json);
  - B's new TLS handshakes and WebSocket upgrades are limited by their own, lower limits;
  - B is served again within RECOVER seconds after it stops;
  - the owner sees the refusals: `pb tunnel status` shows `flood_protection` with a count.
IPv6 /64 keying is covered by TestAddressLimiterStopsOneAddressNotTheCrowd only (no IPv6 observer). Everything it creates it removes.

Usage: flood_rows.py (--machine-cmd '<prefix>' | --windows) --observer-b-cmd '<prefix>' [--pb PATH] [--output FILE]
"""
import argparse
import json
import shlex
import sys
import time
from pathlib import Path

from machine_ctl import Failure, Machine, remote, run
from ip_rules_rows import find_url

ORIGIN_PORT = '9319'
RECOVER = 30
BYSTANDER_BOUND = 20  # seconds a fresh preview may take to serve while another address floods
FLOOD_SECONDS = 20

# runs on observer B: python3 - URL SECONDS
FLOODER = r'''
import http.client, json, ssl, sys, threading, time, urllib.parse
url, seconds = sys.argv[1], float(sys.argv[2])
host = urllib.parse.urlsplit(url).netloc
ctx = ssl.create_default_context()
counts, lock, samples = {}, threading.Lock(), {}
def one(accept):
    conn = http.client.HTTPSConnection(host, 443, timeout=20, context=ctx)
    try:
        conn.request("GET", "/", headers={"Accept": accept})
        r = conn.getresponse(); body = r.read(2000)
        return r.status, r, body
    finally:
        conn.close()
def worker(n):
    # half the workers ask for a name nobody owns under the same domain: the edge answers those without the tunnel, so the
    # address is counted at the edge's own speed (a slow machine behind the tunnel would otherwise never reach the limit)
    target = host if n % 2 == 0 else "flood-%d.%s" % (n, host.split(".", 1)[1])
    conn = http.client.HTTPSConnection(target, 443, timeout=20, context=ctx)
    end = time.time() + seconds
    while time.time() < end:
        try:
            conn.request("GET", "/")
            r = conn.getresponse(); r.read()
            key = str(r.status)
            if r.status == 429:
                samples.setdefault("429", (r.getheader("X-Paperboat-Error"), r.getheader("Retry-After")))
        except Exception as e:
            key = "error"; conn.close(); conn = http.client.HTTPSConnection(target, 443, timeout=20, context=ctx)
        with lock: counts[key] = counts.get(key, 0) + 1
threads = [threading.Thread(target=worker, args=(i,)) for i in range(40)]
[t.start() for t in threads]
time.sleep(seconds * 0.7)
out = {}
for name, accept in (("page", "text/html"), ("api", "application/json")):
    try:
        for _ in range(60):  # ask until one request is the refused one (a new connection may itself be refused at the handshake)
            try:
                status, r, body = one(accept)
            except Exception:
                time.sleep(0.2)
                continue
            if status == 429:
                break
        out[name] = {"status": status, "type": r.getheader("Content-Type"), "error": r.getheader("X-Paperboat-Error"), "retry_after": r.getheader("Retry-After"), "body": body[:60].decode("latin1")}
    except Exception as e:
        out[name] = {"error": str(e)[:100]}
[t.join() for t in threads]
out["counts"], out["sample"] = counts, samples.get("429")
# new TLS handshakes (limit 20/s, burst 50) and WebSocket upgrades (20/s, burst 40) need parallel callers to exceed them
handshake = {"ok": 0, "refused": 0}
def shake():
    for _ in range(12):
        try:
            import socket
            with socket.create_connection((host, 443), timeout=10) as s, ctx.wrap_socket(s, server_hostname=host):
                key = "ok"
        except Exception:
            key = "refused"
        with lock: handshake[key] += 1
ts = [threading.Thread(target=shake) for _ in range(24)]
[t.start() for t in ts]; [t.join() for t in ts]
out["handshakes"] = handshake
upgrades = {}
time.sleep(5)
def upgrade():
    for _ in range(8):
        try:
            conn = http.client.HTTPSConnection(host, 443, timeout=10, context=ctx)
            conn.request("GET", "/", headers={"Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ==", "Sec-WebSocket-Version": "13"})
            r = conn.getresponse(); r.read(); key = str(r.status); conn.close()
        except Exception:
            key = "error"
        with lock: upgrades[key] = upgrades.get(key, 0) + 1
ts = [threading.Thread(target=upgrade) for _ in range(24)]
[t.start() for t in ts]; [t.join() for t in ts]
out["upgrades"] = upgrades
print(json.dumps(out))
'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--machine-cmd', default='')
    parser.add_argument('--windows', action='store_true')
    parser.add_argument('--observer-b-cmd', required=True)
    parser.add_argument('--pb', default='/home/pbt/.local/bin/pb')
    parser.add_argument('--output', default='')
    args = parser.parse_args()
    try:
        machine = Machine(args.machine_cmd, args.pb, args.windows)
    except Failure as error:
        parser.error(str(error))
    observer_b = shlex.split(args.observer_b_cmd)
    name = 'tor-flood-%d' % int(time.time())
    cleanup, steps = [], []

    def good(*arguments, timeout=180):
        code, value = machine.pb(*arguments, '--json', timeout=timeout)
        if code != 0:
            raise Failure('%s: %s' % (' '.join(arguments), str(value)[:300]))
        return value

    def fetch(observer, url):
        code, out, err = run(remote(observer, ['curl', '-sS', '-m', '25', '-o', '/dev/null', '-w', '%{http_code}', url]), 60)
        return int(out) if out.strip().isdigit() else 0

    def step(label, ok, detail=''):
        steps.append({'step': label, 'status': 'PASS' if ok else 'FAIL', 'detail': str(detail)[:400]})
        print(json.dumps(steps[-1]), flush=True)

    try:
        cleanup.append(machine.start_origin(ORIGIN_PORT, 'flood-app'))
        created = good('tunnel', 'create', name, '--port', ORIGIN_PORT, '--duration', '30m', '--wait', '--timeout', '90s', timeout=200)
        tunnel_id, url = created['tunnel']['id'], created['tunnel']['stable_endpoint']

        def delete():
            code, value = machine.pb('tunnel', 'delete', tunnel_id, '--json')
            token = value.get('confirmation_token') if isinstance(value, dict) else None
            if token:
                machine.pb('tunnel', 'delete', tunnel_id, '--confirm', token, '--json')
        cleanup.append(delete)
        deadline = time.time() + 120
        while time.time() < deadline and not (fetch([], url) == 200 and fetch(observer_b, url) == 200):
            time.sleep(3)
        step('baseline: both addresses are served', fetch([], url) == 200 and fetch(observer_b, url) == 200)
        # B floods in the background while A keeps visiting once a second
        import subprocess
        flood = subprocess.Popen(remote(observer_b, ['python3', '-', url, str(FLOOD_SECONDS)]), stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        flood.stdin.write(FLOODER)
        flood.stdin.close()
        during, bystander = [], None
        while flood.poll() is None and len(during) < FLOOD_SECONDS:
            during.append(fetch([], url))
            if bystander is None and len(during) == 4:
                # a bystander's NEW publication while the flood runs: created, then served to a third address within the normal bound
                made = good('preview', ORIGIN_PORT, '--background', '--ttl', '30m', timeout=200)
                preview_id = made.get('id') or (made.get('preview') or {}).get('id')
                cleanup.append(lambda: machine.pb('preview', 'stop', preview_id, '--json'))
                preview_url = find_url(made)
                started = time.time()
                while time.time() - started < BYSTANDER_BOUND and fetch([], preview_url) != 200:
                    time.sleep(1)
                bystander = round(time.time() - started, 1)
            time.sleep(1)
        out, err = flood.stdout.read(), flood.stderr.read()  # the generator has exited: its stdin was closed after the script went in
        try:
            report = json.loads(out.strip().splitlines()[-1])
        except (ValueError, IndexError):
            raise Failure('the flood generator printed no report: %s %s' % (out[-200:], err[-200:]))
        counts = report['counts']
        step('the flooding address is refused 429 edge_rate_limited with Retry-After, after getting its burst',
             counts.get('429', 0) > 0 and counts.get('200', 0) > 0 and report['sample'] and report['sample'][0] == 'edge_rate_limited' and report['sample'][1], '%s %s' % (counts, report['sample']))
        step('a new preview made during the flood is served to another address within %d s' % BYSTANDER_BOUND, bystander is not None and bystander < BYSTANDER_BOUND, '%s s' % bystander)
        step('another address is served every time during the flood', during and all(code == 200 for code in during), during)
        page, api = report['page'], report['api']
        step('a browser gets a page, an API client problem+json',
             page.get('status') == 429 and 'text/html' in str(page.get('type')) and api.get('status') == 429 and 'problem+json' in str(api.get('type')), '%s %s' % (page, api))
        step('new TLS handshakes from the address are limited', report['handshakes']['refused'] > 0, report['handshakes'])
        step('WebSocket upgrades from the address are limited', report['upgrades'].get('429', 0) > 0, report['upgrades'])
        recovered, deadline = 0, time.time() + RECOVER
        while time.time() < deadline and recovered != 200:
            recovered = fetch(observer_b, url)
            time.sleep(2)
        step('the address is served again after it stops (within %d s)' % RECOVER, recovered == 200, recovered)
        seen = None
        deadline = time.time() + 150
        while time.time() < deadline:
            status = good('tunnel', 'status', name)
            seen = status.get('flood_protection') or (status.get('tunnel') or {}).get('flood_protection')
            if seen and seen.get('refused', 0) > 0:
                break
            time.sleep(10)
        step('the owner sees the refusals in pb tunnel status (flood_protection)', bool(seen and seen.get('refused', 0) > 0), seen)
    except Exception as error:  # noqa: BLE001 - the report shows it and the cleanup still runs
        steps.append({'step': 'fixture', 'status': 'FAIL', 'detail': str(error)[:400]})
    finally:
        while cleanup:
            try:
                cleanup.pop()()
            except Exception as error:  # noqa: BLE001
                print('cleanup failed:', str(error)[:200], file=sys.stderr)
    ok = bool(steps) and all(s['status'] == 'PASS' for s in steps)
    report = {'check': 'flood-rows', 'row': 'F8-14', 'status': 'PASS' if ok else 'FAIL', 'steps': steps}
    print(json.dumps(report, indent=1))
    if args.output:
        Path(args.output).write_text(json.dumps(report, indent=1))
    return 0 if ok else 1


if __name__ == '__main__':
    sys.exit(main())
