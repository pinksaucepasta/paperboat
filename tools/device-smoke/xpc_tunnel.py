#!/usr/bin/env python3
"""X-20: tunnels from a machine of each OS behave the same for every origin address, protocol and TLS mode.

  xpc_tunnel.py [--devices L,M,W] [--only bind-v4,bind-v6,bind-all,https,tcp-tls,host-header,preview,grpc,closed-port]

The origins run ON the device (xpc_origin.py, xpc_grpc); `pb tunnel create` publishes them; the controller probes the public URL:
  bind-*       origin on 127.0.0.1 / ::1 / all addresses: marker on HTTP/1.1 and HTTP/2, 16 MiB download (sha256), SSE unbuffered,
               /xff; on bind-v4 also HTTP/3, WebSocket and a 16 MiB POST (preview_protocol_client.go); then the origin stops and
               the edge's error page is recorded (it must be identical on every OS)
  https        HTTPS origin with a self-signed certificate: refused by default, served with --tls-verification pinned --tls-pin
               and with --tls-ca <the certificate's path on the device>
  tcp-tls      public raw TCP echo and TLS passthrough (the certificate that comes back is the origin's)
  host-header  --host-header reaches the origin
  grpc         gRPC over h2c: unary, streaming, grpc-status trailers
  closed-port  a tunnel to a port nothing listens on names the origin in `pb tunnel status`
  preview      `pb preview` of a local origin: HTTP/1.1, HTTP/2, 16 MiB, SSE, HTTP/3, WebSocket, POST, owner IP deny and clear
               (bind-v4 also runs the IP deny on its tunnel)
Results: results/xpc-tunnel-<stamp>.json (`X-20:<check>:<L|M|W>`), tunnels are deleted on every exit path.
"""
import hashlib
import http.client
import json
import re
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import uuid
from pathlib import Path

HERE = Path(__file__).parent
sys.path.insert(0, str(HERE))
import xplat_fleet as fleet  # noqa: E402
from xplat_tunnel import BIG, confirm_delete, curl, endpoint  # noqa: E402

BUILD = Path.home() / '.pbmac/xpc/build'
BINDS = {'v4': '127.0.0.1', 'v6': '::1', 'all': '0.0.0.0'}
results = {}
errors = []


def sep(dev):
    return '\\' if dev.os == 'windows' else '/'


def py(dev):
    return 'python' if dev.os == 'windows' else 'python3'


def port_for(dev, key):
    return 31000 + sum(map(ord, dev.name + key)) % 2000


def put(dev, name, data, mode='755'):
    path = dev.home + sep(dev) + name
    if dev.os == 'windows' and len(data) > 1 << 20:  # the chunked PowerShell upload takes minutes for a binary: scp it
        with tempfile.NamedTemporaryFile() as local:
            local.write(data)
            local.flush()
            subprocess.run(['scp', '-q', local.name, '%s:%s' % (dev.device.target, path.replace('\\', '/'))], check=True, timeout=600)
        return path
    dev.write(path, data, mode=mode)
    return path


def kill_fixtures(dev):
    """Fixtures end themselves after 5 minutes: a leftover on the same port would answer with an old certificate."""
    if dev.os == 'windows':
        dev.sh("Get-CimInstance Win32_Process | Where-Object { $_.CommandLine -like '*xpc_origin.py*' -or $_.Name -eq 'xpc_grpc.exe' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }")
    else:
        dev.sh('pkill -f "[x]pc_origin.py"; pkill -f "[x]pc_grpc"; true')


class Origin:
    """A fixture process on the device, started by xpc_origin.py, stopped by its own stop path or its 5 minute timer."""

    def __init__(self, dev, *args):
        self.dev, self.args = dev, args
        self.script = put(dev, 'xpc_origin.py', (HERE / 'xpc_origin.py').read_bytes())

    def __enter__(self):
        kill_fixtures(self.dev)
        self.thread = threading.Thread(target=lambda: self.dev.run([py(self.dev), self.script, *self.args], timeout=320), daemon=True)
        self.thread.start()
        time.sleep(5)
        return self

    def __exit__(self, *exc):
        kill_fixtures(self.dev)
        return False


def create(dev, name, *args):
    created = fleet.json_pb(dev, 'tunnel', 'create', name, *args, '--wait', timeout=240)
    return created, endpoint(created)


def host_of(url):
    return url.split('//')[1].split('/')[0]


def sse(url):
    context = ssl._create_unverified_context()
    connection = http.client.HTTPSConnection(host_of(url), context=context, timeout=30)
    connection.request('GET', '/sse')
    response = connection.getresponse()
    assert response.status == 200 and 'text/event-stream' in response.getheader('Content-Type', ''), (response.status, response.getheaders())
    stamps = []
    while True:
        line = response.readline()
        if not line:
            break
        if line.startswith(b'data:'):
            stamps.append(time.monotonic())
    assert len(stamps) == 5 and stamps[-1] - stamps[0] >= 0.9, 'SSE arrived buffered or short: %d events over %.2fs' % (len(stamps), (stamps[-1] - stamps[0]) if stamps else 0)
    return '5 events over %.1fs' % (stamps[-1] - stamps[0])


def text_of(page):
    return re.sub(r'\s+', ' ', re.sub(r'<style.*?</style>|<[^>]+>', ' ', page, flags=re.S))[:160]


def marker_h1_h2(url, marker):
    for flag, version in (('--http1.1', '1.1'), ('--http2', '2')):
        out = curl(url + '/', flag).stdout.decode().rsplit('\n', 1)
        assert out[0] == marker and out[1].split()[:2] == ['200', version], '%s %s -> %s | %s' % (flag, url, text_of(out[0]), out[1])


def big_download(url):
    big = curl(url + '/big', '--http1.1', timeout=120).stdout.rsplit(b'\n', 1)[0]
    assert hashlib.sha256(big).hexdigest() == hashlib.sha256(BIG).hexdigest(), 'download differs (%d bytes)' % len(big)


def protocol_client(url, marker):
    done = subprocess.run([str(BUILD / 'probe'), '-url', url, '-marker', marker], capture_output=True, timeout=180)
    assert done.returncode == 0, 'HTTP/3, WebSocket or 16 MiB POST failed: ' + (done.stderr or done.stdout).decode()[-300:]


def t_bind(dev, key):
    bind, port = BINDS[key], port_for(dev, key)
    marker = 'xpc-%s-%s' % (dev.os, key)
    name = 'xpc-' + uuid.uuid4().hex[:8]
    with Origin(dev, 'http', bind, str(port), marker):
        # --port dials 127.0.0.1; `localhost` is dialed as written and reaches an app on ::1 only
        created, url = create(dev, name, *(('--from', 'http://localhost:%d' % port) if key == 'v6' else ('--port', str(port))))
        try:
            retry(lambda: marker_h1_h2(url, marker), 12)  # a connector on a loaded host can take a while after `--wait`
            marker_h1_h2(url, marker)
            big_download(url)
            detail = ['H1+H2 marker', '16 MiB sha256', 'SSE ' + sse(url)]
            xff = json.loads(curl(url + '/xff', '--http1.1').stdout.decode().rsplit('\n', 1)[0])
            assert xff['xff'] and xff['host'] == host_of(url), xff
            detail.append('xff=' + xff['xff'])
            if key == 'v4':
                protocol_client(url, marker)
                detail.append('H3+WebSocket+16 MiB POST')
                detail.append(deny_rule(dev, 'tunnel', created.get('tunnel', created)['id'], url, marker))
            curl(url + '/__stop', '--http1.1')
            time.sleep(4)
            gone = curl(url + '/', '--http1.1').stdout.decode()
            detail.append('stopped -> ' + hashlib.sha256(re.sub(r'\s+', ' ', gone).encode()).hexdigest()[:12])
            return '; '.join(detail)
        finally:
            confirm_delete(dev, name)


def selfsigned(directory):
    cert, key = Path(directory) / 'cert.pem', Path(directory) / 'key.pem'
    subprocess.run(['openssl', 'req', '-x509', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:prime256v1', '-nodes', '-days', '2',
                    '-subj', '/CN=xpc-tls-origin', '-addext', 'subjectAltName=IP:127.0.0.1,DNS:localhost', '-keyout', str(key),
                    '-out', str(cert)], check=True, capture_output=True)
    fp = subprocess.run(['openssl', 'x509', '-in', str(cert), '-noout', '-fingerprint', '-sha256'], capture_output=True, text=True, check=True).stdout
    return cert.read_bytes(), key.read_bytes(), 'sha256:' + fp.split('=')[1].strip().replace(':', '').lower()


def route_id(dev, tunnel):
    return fleet.json_pb(dev, 'tunnel', 'route', 'list', tunnel)['items'][0]['id']


def served(url, marker, label, want=True, tries=15, doctor=None):
    for _ in range(tries):
        out = curl(url + '/', '--http1.1').stdout.decode().rsplit('\n', 1)
        if (out[0] == marker) == want:
            return out
        time.sleep(3)
    why = doctor() if doctor else ''
    raise AssertionError('%s: marker %s after %d tries: %r %s' % (label, 'missing' if want else 'still served', tries, out[1], why[:250]))


def t_https(dev):
    port, marker = port_for(dev, 'https'), 'xpc-https-' + dev.os
    name = 'xpc-' + uuid.uuid4().hex[:8]
    with tempfile.TemporaryDirectory() as directory:
        cert, key, pin = selfsigned(directory)
        cert_path, key_path = put(dev, 'xpc-cert.pem', cert, '644'), put(dev, 'xpc-key.pem', key, '600')
        with Origin(dev, 'https', '127.0.0.1', str(port), marker, cert_path, key_path):
            created, url = create(dev, name, '--from', 'https://127.0.0.1:%d' % port)
            try:
                tunnel = created.get('tunnel', created)['id']
                refused = curl(url + '/', '--http1.1').stdout.decode()
                assert marker not in refused, 'a self-signed origin was served with system verification'
                why = lambda: dev.pb('tunnel', 'doctor', tunnel).text  # noqa: E731
                fleet.json_pb(dev, 'tunnel', 'route', 'update', tunnel, 'default', '--tls-ca', cert_path, '--tls-server-name', 'localhost', '--wait', timeout=120)  # an IP origin presents the public name as SNI unless one is given
                served(url, marker, '--tls-ca', doctor=why)
                fleet.json_pb(dev, 'tunnel', 'route', 'update', tunnel, 'default', '--tls-verification', 'pinned', '--tls-pin', pin, '--wait', timeout=120)
                served(url, marker, 'pinned fingerprint', doctor=why)
                fleet.json_pb(dev, 'tunnel', 'route', 'update', tunnel, 'default', '--tls-verification', 'pinned', '--tls-pin', 'sha256:' + '0' * 64, '--wait', timeout=120)
                served(url, marker, 'wrong pin', want=False)
                curl(url + '/__stop', '--http1.1')
                return 'self-signed origin: refused by default, served with pinned fingerprint, refused with a wrong pin, served with --tls-ca %s and --tls-server-name' % cert_path.split(sep(dev))[-1]
            finally:
                confirm_delete(dev, name)
                for p in (cert_path, key_path):
                    dev.remove(p)


def retry(action, tries=10):
    for attempt in range(tries):
        try:
            return action()
        except Exception:  # noqa: BLE001
            if attempt == tries - 1:
                raise
            time.sleep(4)


def t_tcp_tls(dev):
    import socket
    suffix, tcp_port, tls_port = uuid.uuid4().hex[:8], port_for(dev, 'tcp'), port_for(dev, 'tls')
    made = []
    with tempfile.TemporaryDirectory() as directory:
        cert, key, _ = selfsigned(directory)
        cert_path, key_path = put(dev, 'xpc-cert.pem', cert, '644'), put(dev, 'xpc-key.pem', key, '600')
        try:
            with Origin(dev, 'tcp', '127.0.0.1', str(tcp_port)):
                made.append('xpc-tcp-' + suffix)
                created, _ = create(dev, made[-1], '--from', 'tcp://127.0.0.1:%d' % tcp_port)
                route = fleet.json_pb(dev, 'tunnel', 'route', 'list', made[-1])['items'][0]
                assert route['protocol'] == 'tcp' and route['public_tcp_port'] >= 1024, route

                def through_tcp():
                    with socket.create_connection((route['host_match']['hostname'], route['public_tcp_port']), timeout=15) as c:
                        c.sendall(b'xpc')
                        return c.recv(100)
                assert retry(through_tcp) == b'echo:xpc'
                with socket.create_connection((route['host_match']['hostname'], route['public_tcp_port']), timeout=15) as c:
                    c.sendall(b'STOP\n')
            with Origin(dev, 'tls', '127.0.0.1', str(tls_port), cert_path, key_path):
                made.append('xpc-tls-' + suffix)
                created, _ = create(dev, made[-1], '--port', str(tls_port))
                default = route_id(dev, made[-1])
                token = json.loads(dev.pb('tunnel', 'route', 'remove', made[-1], default, '--json').text)['data']['confirmation_token']
                dev.pb('tunnel', 'route', 'remove', made[-1], default, '--json', '--confirm', token)
                fleet.json_pb(dev, 'tunnel', 'route', 'add', made[-1], '--name', 'tls1', '--protocol', 'tls', '--to', 'tcp://127.0.0.1:%d' % tls_port, '--wait', timeout=240)
                added = fleet.json_pb(dev, 'tunnel', 'route', 'list', made[-1])['items']
                assert [r['protocol'] for r in added] == ['tls'] and added[0]['public_tcp_port'] == 443, added
                host = added[0]['host_match']['hostname']
                client = ssl.create_default_context()
                client.check_hostname, client.verify_mode = False, ssl.CERT_NONE

                def through_tls():
                    with socket.create_connection((host, 443), timeout=15) as plain, client.wrap_socket(plain, server_hostname=host) as secured:
                        secured.sendall(b'xpc')
                        return secured.recv(100), ssl.DER_cert_to_PEM_cert(secured.getpeercert(binary_form=True))
                payload, pem = retry(through_tls)
                assert payload == b'tls-origin-ok' and pem.strip() == cert.decode().strip(), 'passthrough did not deliver the origin certificate'
            return 'public raw TCP echo through :%d; TLS passthrough on :443 delivered the origin certificate' % route['public_tcp_port']
        finally:
            for tunnel in made:
                confirm_delete(dev, tunnel)
            for p in (cert_path, key_path):
                dev.remove(p)


def t_host_header(dev):
    port, marker = port_for(dev, 'host'), 'xpc-host-' + dev.os
    name = 'xpc-' + uuid.uuid4().hex[:8]
    with Origin(dev, 'http', '127.0.0.1', str(port), marker):
        created, url = create(dev, name, '--port', str(port))
        try:
            tunnel = created.get('tunnel', created)['id']
            served(url, marker, 'default route')
            fleet.json_pb(dev, 'tunnel', 'route', 'update', tunnel, 'default', '--host-header', 'xpc.example.test', '--wait', timeout=120)
            for _ in range(8):
                try:
                    seen = json.loads(curl(url + '/xff', '--http1.1').stdout.decode().rsplit('\n', 1)[0]).get('host')
                except ValueError:
                    seen = None
                if seen == 'xpc.example.test':
                    break
                time.sleep(3)
            assert seen == 'xpc.example.test', 'origin saw Host %r' % seen
            curl(url + '/__stop', '--http1.1')
            return '--host-header xpc.example.test reached the origin'
        finally:
            confirm_delete(dev, name)


def t_grpc(dev):
    port, name = port_for(dev, 'grpc'), 'xpc-' + uuid.uuid4().hex[:8]
    binary = {'linux': 'grpc-linux', 'darwin': 'grpc-darwin', 'windows': 'grpc-windows.exe'}[dev.os]
    path = put(dev, 'xpc_grpc' + ('.exe' if dev.os == 'windows' else ''), (BUILD / binary).read_bytes())
    kill_fixtures(dev)
    thread = threading.Thread(target=lambda: dev.run([path, 'serve', '127.0.0.1:%d' % port], timeout=330), daemon=True)
    thread.start()
    time.sleep(5)
    created, url = create(dev, name, '--from', 'h2c://127.0.0.1:%d' % port)
    try:
        time.sleep(5)
        done = subprocess.run([str(BUILD / 'grpc-linux'), 'probe', url], capture_output=True, timeout=120)
        assert done.returncode == 0, (done.stderr or done.stdout).decode()[-300:]
        return 'gRPC over h2c: unary, 3-message stream, grpc-status trailers 0 and 5'
    finally:
        confirm_delete(dev, name)
        kill_fixtures(dev)


def t_closed_port(dev):
    port, name = port_for(dev, 'closed'), 'xpc-' + uuid.uuid4().hex[:8]
    created, url = create(dev, name, '--port', str(port))
    try:
        for _ in range(36):  # the connector reports an unreachable origin within 150 s (tunnel_failures.py)
            status = dev.pb('tunnel', 'status', name).text
            if 'unreachable' in status.lower() or str(port) in status:
                break
            time.sleep(5)
        assert 'unreachable' in status.lower() or str(port) in status, 'status does not name the origin: ' + status[-300:]
        page = curl(url + '/', '--http1.1').stdout.decode()
        return 'status names origin 127.0.0.1:%d; edge page sha %s' % (port, hashlib.sha256(re.sub(r'\s+', ' ', page).encode()).hexdigest()[:12])
    finally:
        confirm_delete(dev, name)


def deny_rule(dev, kind, ident, url, marker):
    """The owner's IP deny list sees the client address the edge saw (/xff): denied -> refused, cleared -> served again."""
    xff = json.loads(curl(url + '/xff', '--http1.1').stdout.decode().rsplit('\n', 1)[0])['xff'].split(',')[0].strip()
    fleet.json_pb(dev, kind, 'ip-rules', ident, '--deny', xff)
    try:
        served(url, marker, 'deny ' + xff, want=False, tries=8)
    finally:
        fleet.json_pb(dev, kind, 'ip-rules', ident, '--clear')
    served(url, marker, 'cleared rules', tries=8)
    return 'owner deny of %s refused the visitor, clear served it again' % xff


def t_preview(dev):
    port, marker = port_for(dev, 'preview'), 'xpc-preview-' + dev.os
    with Origin(dev, 'http', '127.0.0.1', str(port), marker):
        done = dev.pb('preview', str(port), '--background', '--ttl', '10m', '--json', timeout=180)
        urls, ident = re.findall(r'https://[A-Za-z0-9._-]+', done.text), re.search(r'"id"\s*:\s*"([^"]+)"', done.text)
        assert done.rc == 0 and urls and ident, 'preview: ' + (done.text + done.errtext)[-300:]
        url = urls[0]
        try:
            retry(lambda: marker_h1_h2(url, marker), 12)
            big_download(url)
            detail = ['H1+H2 marker', '16 MiB sha256', 'SSE ' + sse(url)]
            protocol_client(url, marker)
            detail.append('H3+WebSocket+16 MiB POST')
            detail.append(deny_rule(dev, 'preview', ident.group(1), url, marker))
            return '; '.join(detail)
        finally:
            dev.pb('preview', 'stop', ident.group(1), '--json', timeout=60)
            kill_fixtures(dev)


CHECKS = {'bind-v4': lambda d: t_bind(d, 'v4'), 'bind-v6': lambda d: t_bind(d, 'v6'), 'bind-all': lambda d: t_bind(d, 'all'),
          'https': t_https, 'tcp-tls': t_tcp_tls, 'host-header': t_host_header, 'preview': t_preview, 'grpc': t_grpc, 'closed-port': t_closed_port}


def main():
    argv = sys.argv[1:]
    keys = argv[argv.index('--devices') + 1].split(',') if '--devices' in argv else ['L', 'M', 'W']
    only = argv[argv.index('--only') + 1].split(',') if '--only' in argv else list(CHECKS)
    for key, dev in {k: d for k, d in fleet.fleet().items() if k in keys}.items():
        for check in only:
            name, started = 'X-20:%s:%s' % (check, key), time.time()
            try:
                detail = CHECKS[check](dev)
                results[name] = {'status': 'PASS', 'detail': detail}
            except Exception as error:  # noqa: BLE001
                results[name] = {'status': 'FAIL', 'detail': str(error)[:700]}
            print('%s %s %ds %s' % (results[name]['status'], name, time.time() - started, results[name]['detail'][:400]), flush=True)
    out = HERE / 'results' / ('xpc-tunnel-%s.json' % time.strftime('%Y%m%dT%H%M%SZ', time.gmtime()))
    out.parent.mkdir(exist_ok=True)
    out.write_text(json.dumps({'check': 'xpc-tunnel', 'devices': keys, 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
