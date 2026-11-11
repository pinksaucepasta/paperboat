#!/usr/bin/env python3
"""X-19: private TCP between OSes — `pb access machine` forward and the assigned 127.100.x.y address, byte-exact with half-close.

  xplat_access.py [--devices L,M,W]

For every ordered pair (client A, target B) one Python driver runs ON A (python3 on L and M, python on W): it starts an origin
on B through `pb exec B`, opens `pb access machine B --port P --listen 127.0.0.1:Q`, sends 1 MiB, half-closes, and reads the
origin's reply (a digest of what it received) back; then does the same through B's own private address from `pb resolve B`.
It also checks `--listen` refuses a non-loopback address. Results: tools/device-smoke/results/xplat-access-<stamp>.json.
"""
import json
import sys
import time
from itertools import permutations
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import xplat_fleet as fleet  # noqa: E402

results = {}

ORIGIN = r'''
import hashlib, socket, sys
srv = socket.socket(); srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", int(sys.argv[1]))); srv.listen(4); srv.settimeout(90)
while True:
    c, _ = srv.accept(); c.settimeout(30); h = hashlib.sha256(); n = 0
    while True:
        b = c.recv(65536)
        if not b: break
        h.update(b); n += len(b)
    c.sendall(("%d %s\n" % (n, h.hexdigest())).encode()); c.shutdown(socket.SHUT_WR); c.close()
'''

DRIVER = r'''
import hashlib, json, os, socket, subprocess, sys, time
pb, machine, origin_port, listen_port, origin_code, origin_py = sys.argv[1:7]
PAYLOAD = (("private TCP λ船\n".encode() + bytes(range(256))) * 4096)[:1 << 20]
want = "%d %s" % (len(PAYLOAD), hashlib.sha256(PAYLOAD).hexdigest())
quiet = dict(stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
def exchange(address, port):
    s = socket.create_connection((address, int(port)), timeout=30)
    s.sendall(PAYLOAD); s.shutdown(socket.SHUT_WR)
    got = b""
    while True:
        b = s.recv(4096)
        if not b: break
        got += b
    s.close(); return got.decode().strip()
out = {}
origin = subprocess.Popen([pb, "exec", machine, "--", origin_py, "-c", origin_code, origin_port], **quiet)
access = None
try:
    time.sleep(6)
    bad = subprocess.run([pb, "access", "machine", machine, "--port", origin_port, "--listen", "192.0.2.1:%s" % listen_port], capture_output=True, text=True, timeout=60)
    out["non_loopback_refused"] = bad.returncode != 0
    out["non_loopback_message"] = (bad.stderr + bad.stdout).strip()[-160:]
    access = subprocess.Popen([pb, "access", "machine", machine, "--port", origin_port, "--listen", "127.0.0.1:" + listen_port], **quiet)
    deadline = time.time() + 40
    while time.time() < deadline:
        try:
            socket.create_connection(("127.0.0.1", int(listen_port)), timeout=2).close(); break
        except OSError:
            time.sleep(1)
    time.sleep(1)
    out["forward"] = exchange("127.0.0.1", listen_port) == want
    resolved = subprocess.run([pb, "resolve", machine, "--json"], capture_output=True, text=True, timeout=60)
    try:
        data = json.loads(resolved.stdout); data = data.get("data", data)
        address = data.get("address") or (data.get("addresses") or [None])[0]
    except Exception:
        address = None
    out["address"] = address
    out["private_address"] = bool(address) and exchange(address, origin_port) == want
finally:
    for p in (access, origin):
        if p and p.poll() is None:
            p.terminate()
            try: p.wait(10)
            except Exception: p.kill()
print(json.dumps(out))
'''


def py(dev):
    return 'python' if dev.os == 'windows' else 'python3'


def t_access(src, dst):
    port = 21000 + sum(map(ord, src.name + dst.name)) % 2000
    done = src.run([py(src), '-c', DRIVER, src.pb_path, dst.alias, str(port), str(port + 5000), ORIGIN, py(dst)], timeout=240)
    assert done.rc == 0, 'driver: %s %s' % (done.text[-300:], done.errtext[-300:])
    out = json.loads(done.text.strip().splitlines()[-1])
    assert out.get('forward'), 'the 1 MiB exchange through the forward was not byte-exact with half-close: %s' % out
    assert out.get('private_address'), 'the exchange through the private address %s failed: %s' % (out.get('address'), out)
    assert out.get('non_loopback_refused'), '--listen accepted a non-loopback address: %s' % out
    return 'forward and %s byte-exact with half-close; non-loopback --listen refused (%s)' % (out['address'], out['non_loopback_message'][:70])


def check(name, pair, action):
    key = 'X-19:%s:%s' % (name, pair)
    started = time.time()
    try:
        detail = action()
        results[key] = {'status': 'PASS', 'detail': detail or ''}
        print('PASS %s %ds %s' % (key, time.time() - started, (detail or '')[:300]), flush=True)
    except Exception as error:  # noqa: BLE001
        results[key] = {'status': 'FAIL', 'detail': str(error)[:700]}
        print('FAIL %s %ds: %s' % (key, time.time() - started, str(error)[:700]), flush=True)


def main():
    argv = sys.argv[1:]
    keys = argv[argv.index('--devices') + 1].split(',') if '--devices' in argv else ['L', 'M', 'W']
    devices = {k: d for k, d in fleet.fleet().items() if k in keys}
    for a_key, b_key in permutations(devices, 2):
        check('tcp', '%s>%s' % (a_key, b_key), lambda: t_access(devices[a_key], devices[b_key]))
    stamp = time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())
    out = Path(__file__).parent / 'results' / ('xplat-access-%s.json' % stamp)
    out.write_text(json.dumps({'check': 'xplat-access', 'devices': keys, 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
