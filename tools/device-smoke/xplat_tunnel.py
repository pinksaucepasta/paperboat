#!/usr/bin/env python3
"""X-20: a tunnel from a machine of each OS reaches an origin that listens on 127.0.0.1 only, on ::1 only and on all addresses.

  xplat_tunnel.py [--devices L,M,W] [--only v4,v6,all]

For each device and bind address one Python origin runs ON the device (HTTP/1.1: a marker page, a 16 MiB download and a
stop path), `pb tunnel create --port` publishes it, and the controller fetches the public URL over HTTP/1.1 and HTTP/2:
the marker, the sha256 of the download; then it stops the origin through the tunnel and records the error page the edge shows
(it must be the same on every OS). The tunnel is deleted on every exit path. Results:
tools/device-smoke/results/xplat-tunnel-<stamp>.json (`X-20:v6:W`).
"""
import hashlib
import json
import re
import subprocess
import sys
import threading
import time
import uuid
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import xplat_fleet as fleet  # noqa: E402

results = {}
ORIGIN = r'''
import hashlib, http.server, socket, socketserver, sys, threading
bind, port, marker = sys.argv[1], int(sys.argv[2]), sys.argv[3]
BIG = (b"0123456789abcdef" * 64 + b"\n") * 16384
class Server(socketserver.ThreadingMixIn, http.server.HTTPServer):
    address_family = socket.AF_INET6 if ":" in bind else socket.AF_INET
    daemon_threads = True
class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *a): pass
    def reply(self, body, ctype="text/plain"):
        self.send_response(200); self.send_header("Content-Type", ctype); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_GET(self):
        if self.path == "/__stop":
            self.reply(b"stopping"); threading.Thread(target=srv.shutdown, daemon=True).start(); return
        if self.path == "/big": self.reply(BIG, "application/octet-stream"); return
        self.reply(marker.encode())
srv = Server((bind, port), H)
threading.Timer(280, srv.shutdown).start()
srv.serve_forever()
'''
BIG = (b"0123456789abcdef" * 64 + b"\n") * 16384
BINDS = {'v4': '127.0.0.1', 'v6': '::1', 'all': '0.0.0.0'}


def py(dev):
    return 'python' if dev.os == 'windows' else 'python3'


def curl(url, *args, timeout=60):
    return subprocess.run(['curl', '-sS', '-m', str(timeout), '-k', '-w', '\n%{http_code} %{http_version}', *args, url], capture_output=True, timeout=timeout + 10)


def endpoint(created):
    text = json.dumps(created)
    urls = re.findall(r'https://[A-Za-z0-9._-]+', text)
    assert urls, 'no public URL in the tunnel result: ' + text[:300]
    return urls[0]


def confirm_delete(dev, name):
    first = dev.pb('tunnel', 'delete', name, '--json', timeout=120)
    try:
        data = json.loads(first.text)
        token = (data.get('data') or data).get('confirmation_token') or data.get('confirmation_token')
    except ValueError:
        token = None
    if token:
        dev.pb('tunnel', 'delete', name, '--confirm', token, '--json', timeout=120)


def t_origin(dev, key):
    bind, port = BINDS[key], 31000 + sum(map(ord, dev.name + key)) % 2000
    marker = 'xplat-%s-%s' % (dev.os, key)
    name = 'xplat-' + uuid.uuid4().hex[:8]
    origin = threading.Thread(target=lambda: dev.run([py(dev), '-c', ORIGIN, bind, str(port), marker], timeout=300), daemon=True)
    origin.start()
    time.sleep(5)
    # --port dials 127.0.0.1; `localhost` is dialed as written and reaches an app on ::1 only (as in xpc_tunnel.py)
    origin_flag = ('--from', 'http://localhost:%d' % port) if key == 'v6' else ('--port', str(port))
    created = fleet.json_pb(dev, 'tunnel', 'create', name, *origin_flag, '--wait', timeout=240)
    try:
        url = endpoint(created)
        time.sleep(5)
        for flag, version in (('--http1.1', '1.1'), ('--http2', '2')):
            out = curl(url + '/', flag).stdout.decode().rsplit('\n', 1)
            assert out[0] == marker and out[1].split()[0] == '200' and out[1].split()[1] == version, '%s %s -> %r' % (flag, url, out)
        big = curl(url + '/big', '--http1.1', timeout=120).stdout.rsplit(b'\n', 1)[0]
        assert hashlib.sha256(big).hexdigest() == hashlib.sha256(BIG).hexdigest(), 'download differs (%d bytes)' % len(big)
        curl(url + '/__stop', '--http1.1')
        time.sleep(4)
        gone = curl(url + '/', '--http1.1').stdout.decode()
        return 'public %s: marker on HTTP/1.1 and HTTP/2, 16 MiB sha256 equal; origin stopped -> %r' % (url.split('//')[1][:12], gone.replace('\n', ' ')[:90])
    finally:
        confirm_delete(dev, name)


def check(key, dev, action):
    name = 'X-20:%s:%s' % (key, dev.key)
    started = time.time()
    try:
        detail = action()
        results[name] = {'status': 'PASS', 'detail': detail}
        print('PASS %s %ds %s' % (name, time.time() - started, detail[:300]), flush=True)
    except Exception as error:  # noqa: BLE001
        results[name] = {'status': 'FAIL', 'detail': str(error)[:700]}
        print('FAIL %s %ds: %s' % (name, time.time() - started, str(error)[:700]), flush=True)


def main():
    argv = sys.argv[1:]
    keys = argv[argv.index('--devices') + 1].split(',') if '--devices' in argv else ['L', 'M', 'W']
    only = argv[argv.index('--only') + 1].split(',') if '--only' in argv else list(BINDS)
    devices = {k: d for k, d in fleet.fleet().items() if k in keys}
    for key, dev in devices.items():
        dev.key = key
        for bind in only:
            check(bind, dev, lambda: t_origin(dev, bind))
    stamp = time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())
    out = Path(__file__).parent / 'results' / ('xplat-tunnel-%s.json' % stamp)
    out.write_text(json.dumps({'check': 'xplat-tunnel', 'devices': keys, 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
