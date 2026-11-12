#!/usr/bin/env python3
"""F8-09 / F8-10 / F8-11 / F8-12: what a visitor and the owner see through a REAL tunnel when the app behind it is
unusual (origin TLS in every verification mode, gRPC, quiet WebSockets, Node-style keep-alive, failure pages).

Runs ON the device under test (Linux, macOS or Windows; stdlib plus a venv it builds for cryptography and grpcio),
as the user that owns the paired `pb`. The origins are threads of this process on 127.0.0.1, every visitor request goes
to the tunnel's public https:// name, so the production edge, the carrier and this OS's agent path are all in the way.

  tunnel_origin_rows.py --pb PATH --out FILE [--only SCENARIO ...] [--ws-quiet 600] [--slow 105]

Scenarios (each owns one tunnel, they run in parallel): headers (forwarded headers, cookie/redirect/header fidelity, redirect
loop page, 413, 431, ACME on port 80), keepalive, ws (a WebSocket quiet for --ws-quiet seconds), tls (insecure, pinned, wrong
pin, --tls-ca, SNI, and the failure pages with the owner's `pb tunnel doctor` hint for self-signed, expired and unknown CA),
grpc (h2c and TLS origins: unary, streaming, grpc-status trailers), failures (origin refused, 504 after --slow seconds).
Prints one JSON line per cell and writes them all to FILE; exit 1 when a cell failed. Tunnels carry the prefix tor-.
"""
import argparse
import base64
import datetime
import hashlib
import http.client
import http.server
import json
import os
import socket
import ssl
import struct
import subprocess
import sys
import tempfile
import threading
import time

VENV_MARK = 'TOR_VENV'
BOTH_EDGES = 30  # seconds a route may take to be served by the second edge after the CLI returned


def bootstrap():
    """Re-run under a venv that has the libraries this check needs (cryptography, grpcio); building it once per device."""
    if os.environ.get(VENV_MARK):
        return
    home = os.path.join(tempfile.gettempdir(), 'tor-venv')
    python = os.path.join(home, 'Scripts' if os.name == 'nt' else 'bin', 'python' + ('.exe' if os.name == 'nt' else ''))
    if not os.path.exists(python):
        subprocess.run([sys.executable, '-m', 'venv', home], check=True)
        subprocess.run([python, '-m', 'pip', 'install', '-q', 'cryptography', 'grpcio', 'grpcio-health-checking'], check=True)
    sys.exit(subprocess.run([python, *sys.argv], env=dict(os.environ, **{VENV_MARK: '1'})).returncode)


bootstrap()

import grpc  # noqa: E402
from cryptography import x509  # noqa: E402
from cryptography.hazmat.primitives import hashes, serialization  # noqa: E402
from cryptography.hazmat.primitives.asymmetric import ec  # noqa: E402
from cryptography.x509.oid import NameOID  # noqa: E402
from grpc_health.v1 import health, health_pb2, health_pb2_grpc  # noqa: E402

ARGS = None
CELLS = []
LATE = []  # requests that needed a retry because the edge did not yet serve the route
LOCK = threading.Lock()
TUNNELS = []
SUFFIX = time.strftime('%d%H%M') + sys.platform[:2] + os.urandom(2).hex()  # tunnel names are account-wide and a deleted tunnel keeps its name
WORK = tempfile.mkdtemp(prefix='tor-')
SPOOF = {'X-Forwarded-For': '203.0.113.9', 'X-Forwarded-Host': 'evil.example.test', 'X-Forwarded-Proto': 'http',
         'Forwarded': 'for=203.0.113.9;host=evil.example.test'}


def cell(scenario, name, ok, detail=''):
    record = {'scenario': scenario, 'cell': name, 'result': 'ok' if ok else 'FAILED', 'detail': str(detail)[:300]}
    with LOCK:
        CELLS.append(record)
        print(json.dumps(record), flush=True)
    return ok


def pb(*argv, timeout=300, text=False, check=True):
    """pb ARGV --json -> data; text=True runs without --json and returns stdout+stderr."""
    done = subprocess.run([ARGS.pb, *argv] + ([] if text else ['--json']), capture_output=True, text=True, timeout=timeout)
    if text:
        return done.stdout + done.stderr
    try:
        value = json.loads(done.stdout)
    except ValueError:
        value = None
    if value is None or (done.returncode and not (isinstance(value, dict) and value.get('ok') is not False and 'error' not in value)):
        if check:
            raise RuntimeError('pb %s failed: %s' % (' '.join(argv[:3]), (done.stderr or done.stdout)[-300:]))
        return value or {}
    return value.get('data', value) if isinstance(value, dict) and value.get('ok') else value


# --- tunnels ------------------------------------------------------------------------------------------------
def create(scenario, *origin):
    name = 'tor-%s-%s' % (SUFFIX, scenario)
    data = pb('tunnel', 'create', name, *origin, '--duration', '3h', '--wait', '--timeout', '90s', timeout=240)
    TUNNELS.append(name)
    host = data['tunnel']['stable_endpoint'].split('://', 1)[1].rstrip('/')
    return name, host, pb('tunnel', 'route', 'list', name)['items'][0]['id']


def move(name, route, origin, *flags):
    pb('tunnel', 'route', 'update', name, route, '--to', origin, *flags, '--wait', '--timeout', '90s', timeout=240)
    time.sleep(4)


def delete_all():
    for name in TUNNELS:
        try:
            preview = pb('tunnel', 'delete', name, check=False)
            token = (preview.get('data') or preview)['confirmation_token']
            pb('tunnel', 'delete', name, '--confirm', token, check=False)
        except Exception as exc:
            print('cleanup: tunnel %s not deleted: %s' % (name, exc), file=sys.stderr)


# --- visitors -----------------------------------------------------------------------------------------------
def visit(host, path='/', method='GET', headers=None, body=None, timeout=60, retries=15):
    """One HTTPS request to the public name; returns (status, headers, body). The first request after a route moved can
    reach the edge before every node knows the new origin, so an upstream_unavailable answer is retried."""
    for attempt in range(retries):
        conn = http.client.HTTPSConnection(host, 443, timeout=timeout, context=ssl.create_default_context())
        try:
            conn.request(method, path, body=body, headers=headers or {})
            response = conn.getresponse()
            data = response.read(1 << 16)
            result = (response.status, response, data)
        finally:
            conn.close()
        if not (result[0] == 502 and result[1].getheader('X-Paperboat-Error') == 'upstream_unavailable') or attempt == retries - 1:
            if attempt:
                LATE.append({'host': host, 'path': path, 'tries': attempt + 1, 'status': result[0]})
            return result
        time.sleep(4)


def edge_ips(host):
    return sorted({entry[4][0] for entry in socket.getaddrinfo(host, 443, socket.AF_INET)})


class PinnedConnection(http.client.HTTPSConnection):
    """HTTPS to the public name through one chosen edge address."""

    def __init__(self, host, ip, **kwargs):
        super().__init__(host, 443, **kwargs)
        self.ip = ip

    def connect(self):
        sock = socket.create_connection((self.ip, 443), self.timeout)
        self.sock = self._context.wrap_socket(sock, server_hostname=self.host)


def status_via(host, ip, path='/'):
    conn = PinnedConnection(host, ip, timeout=20, context=ssl.create_default_context())
    try:
        conn.request('GET', path)
        response = conn.getresponse()
        response.read(4096)
        return response.status
    except OSError:
        return 0
    finally:
        conn.close()


def serving_ip(host, seconds=120):
    """An edge address that answers 200 for the name (DNS gives both edges; a route can reach one before the other)."""
    deadline = time.time() + seconds
    while time.time() < deadline:
        for ip in edge_ips(host):
            if status_via(host, ip) == 200:
                return ip
        time.sleep(3)
    raise RuntimeError('no edge address serves %s' % host)


def raw_tls(host, request, read=4096, timeout=30):
    with socket.create_connection((host, 443), timeout=timeout) as sock:
        with ssl.create_default_context().wrap_socket(sock, server_hostname=host) as tls:
            tls.sendall(request)
            return tls.recv(read)


# --- certificates -------------------------------------------------------------------------------------------
def make_cert(common_name, names=(), issuer=None, valid=(-1, 2), ca=False):
    """(cert_pem, key_pem, certificate, key). issuer is (certificate, key) of the signing CA, None for self-signed.
    valid is (days before now, days after now) for not-before and not-after."""
    key = ec.generate_private_key(ec.SECP256R1())
    subject = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, common_name)])
    now = datetime.datetime.now(datetime.timezone.utc)
    builder = (x509.CertificateBuilder().subject_name(subject).issuer_name(issuer[0].subject if issuer else subject)
               .public_key(key.public_key()).serial_number(x509.random_serial_number())
               .not_valid_before(now + datetime.timedelta(days=valid[0])).not_valid_after(now + datetime.timedelta(days=valid[1])))
    if ca:
        builder = builder.add_extension(x509.BasicConstraints(ca=True, path_length=None), critical=True)
    if names:
        builder = builder.add_extension(x509.SubjectAlternativeName([x509.DNSName(n) for n in names]), critical=False)
    certificate = builder.sign(issuer[1] if issuer else key, hashes.SHA256())
    pem = certificate.public_bytes(serialization.Encoding.PEM)
    key_pem = key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, serialization.NoEncryption())
    return pem, key_pem, certificate, key


def write(name, data):
    path = os.path.join(WORK, name)
    with open(path, 'wb') as out:
        out.write(data)
    return path


def fingerprint(certificate):
    return 'sha256:' + hashlib.sha256(certificate.public_bytes(serialization.Encoding.DER)).hexdigest()


# --- origins ------------------------------------------------------------------------------------------------
class Origin(http.server.BaseHTTPRequestHandler):
    """The app: echoes what the edge sent and has a path for each behaviour a check needs."""
    protocol_version = 'HTTP/1.1'
    sni = []  # names the TLS handshake carried (https origins)

    def log_message(self, *args):
        pass

    def reply(self, status, body=b'', headers=()):
        self.send_response(status)
        for key, value in headers:
            self.send_header(key, value)
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        size = len(self.rfile.read(int(self.headers.get('Content-Length', 0))))
        self.reply(200, json.dumps({'received': size}).encode())

    def do_GET(self):
        path = self.path.split('?')[0]
        port = self.server.server_address[1]
        if path == '/force':  # a proxy that forces https: always redirects to its own https URL
            return self.reply(301, b'', [('Location', 'https://%s%s' % (self.headers.get('Host'), self.path))])
        if path == '/fidelity':
            return self.reply(200, b'fidelity-body', [
                ('Set-Cookie', 'a=1; Path=/x; HttpOnly; SameSite=Lax'), ('Set-Cookie', 'b=2; Path=/; Secure; Max-Age=60'),
                ('X-Custom', 'kept as sent'), ('Cache-Control', 'private, max-age=7'), ('ETag', '"abc-123"'), ('Vary', 'Accept-Encoding')])
        if path == '/redir':
            return self.reply(302, b'', [('Location', '/next?x=1&y=%2F')])
        if path == '/redir-abs':
            return self.reply(302, b'', [('Location', 'http://127.0.0.1:%d/abs' % port)])
        if path == '/slow':
            time.sleep(float(self.path.split('seconds=')[1]))
            return self.reply(200, b'late')
        if path == '/ws':
            return self.websocket()
        h = self.headers
        self.reply(200, json.dumps({'host': h.get('Host'), 'xfp': h.get('X-Forwarded-Proto'), 'xfh': h.get('X-Forwarded-Host'),
                                    'xff': h.get('X-Forwarded-For'), 'forwarded': h.get('Forwarded'), 'sni': self.sni[-1:] or None}).encode(),
                   [('Content-Type', 'application/json')])

    def websocket(self):
        accept = base64.b64encode(hashlib.sha1((self.headers['Sec-WebSocket-Key'] + '258EAFA5-E914-47DA-95CA-C5AB0DC85B11').encode()).digest()).decode()
        self.send_response(101)
        self.send_header('Upgrade', 'websocket')
        self.send_header('Connection', 'Upgrade')
        self.send_header('Sec-WebSocket-Accept', accept)
        self.end_headers()
        self.wfile.flush()
        self.close_connection = True
        while True:
            frame = read_frame(self.rfile)
            if frame is None or frame[0] == 8:
                return
            if frame[0] == 1:
                self.wfile.write(frame_bytes(1, frame[1]))
                self.wfile.flush()


class KeepAliveOrigin(Origin):
    """Node's http server closes an idle keep-alive connection after 5 s; this one after 1 s so the race is fast."""
    timeout = 1


def read_frame(stream):
    head = stream.read(2)
    if len(head) < 2:
        return None
    opcode, length = head[0] & 15, head[1] & 127
    if length == 126:
        length = struct.unpack('>H', stream.read(2))[0]
    elif length == 127:
        length = struct.unpack('>Q', stream.read(8))[0]
    mask = stream.read(4) if head[1] & 128 else None
    data = stream.read(length)
    if mask:
        data = bytes(byte ^ mask[index % 4] for index, byte in enumerate(data))
    return opcode, data


def frame_bytes(opcode, data, mask=False):
    out = bytes([128 | opcode]) + bytes([(128 if mask else 0) | len(data)])
    if not mask:
        return out + data
    key = os.urandom(4)
    return out + key + bytes(byte ^ key[index % 4] for index, byte in enumerate(data))


class Server(http.server.ThreadingHTTPServer):
    """stop() ends the app the way a crash does: the listener and every open connection go, so nothing keeps answering."""
    daemon_threads = True

    def __init__(self, *args):
        super().__init__(*args)
        self.open_connections = set()

    def process_request(self, request, client_address):
        self.open_connections.add(request)
        super().process_request(request, client_address)

    def stop(self):
        self.shutdown()
        self.server_close()
        for connection in list(self.open_connections):
            try:
                connection.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            connection.close()


def serve(handler, tls=None, port=0):
    """An origin on 127.0.0.1 in a thread; tls is (cert_pem_path, key_pem_path). Returns the server."""
    server = Server(('127.0.0.1', port), handler)
    if tls:
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(*tls)
        context.set_alpn_protocols(['http/1.1'])
        context.sni_callback = lambda sock, name, ctx: Origin.sni.append(name)
        server.socket = context.wrap_socket(server.socket, server_side=True)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


# --- scenarios ----------------------------------------------------------------------------------------------
def headers_scenario():
    s = 'headers'
    origin = serve(Origin)
    name, host, route = create(s, '--port', str(origin.server_address[1]))
    status, response, body = visit(host, '/', headers=SPOOF)
    seen = json.loads(body) if status == 200 else {}
    good = (status == 200 and seen.get('xfp') == 'https' and host in str(seen.get('xfh')) and 'evil.example.test' not in str(seen.get('xfh')) + str(seen.get('host'))
            and '203.0.113.9' not in str(seen.get('xff')) + str(seen.get('forwarded')) and ',' not in str(seen.get('xff')))
    cell(s, 'F8-09 forwarded headers: spoofs dropped, https, public host, one client IP', good, '%s %s' % (status, seen))
    status, response, body = visit(host, '/fidelity')
    cookies = response.headers.get_all('Set-Cookie') or []
    cell(s, 'F8-09 the app\'s cookies, validators and headers arrive unchanged',
         status == 200 and body == b'fidelity-body' and cookies == ['a=1; Path=/x; HttpOnly; SameSite=Lax', 'b=2; Path=/; Secure; Max-Age=60']
         and response.getheader('X-Custom') == 'kept as sent' and response.getheader('Cache-Control') == 'private, max-age=7'
         and response.getheader('ETag') == '"abc-123"' and response.getheader('Vary') == 'Accept-Encoding', '%s %s' % (status, cookies))
    status, response, _ = visit(host, '/redir')
    absolute, absolute_resp, _ = visit(host, '/redir-abs')
    cell(s, 'F8-09 the app\'s redirects (relative and absolute Location) are not rewritten',
         status == 302 and response.getheader('Location') == '/next?x=1&y=%2F' and absolute == 302
         and absolute_resp.getheader('Location') == 'http://127.0.0.1:%d/abs' % origin.server_address[1],
         '%s %s / %s %s' % (status, response.getheader('Location'), absolute, absolute_resp.getheader('Location')))
    status, response, body = visit(host, '/force')
    cell(s, 'F8-09/F8-12 an app that redirects to its own https URL gets the loop page naming the fix',
         status == 502 and response.getheader('X-Paperboat-Error') == 'origin_redirect_loop' and b'HTTP-to-HTTPS redirect' in body, '%s %s' % (status, response.getheader('X-Paperboat-Error')))
    reply = raw_tls(host, ('POST /upload HTTP/1.1\r\nHost: %s\r\nContent-Length: %d\r\n\r\n' % (host, 1 << 40)).encode())
    cell(s, 'F8-12 a body over the cap is 413 request_body_too_large', reply.startswith(b'HTTP/1.1 413') and b'request_body_too_large' in reply, reply[:80])
    reply = raw_tls(host, ('GET / HTTP/1.1\r\nHost: %s\r\n' % host + ''.join('X-Pad-%d: %s\r\n' % (i, 'a' * 1000) for i in range(40)) + '\r\n').encode())
    cell(s, 'F8-12 oversized headers are 431, not 502 or 404', reply.startswith(b'HTTP/1.1 431'), reply[:80])
    acme = http.client.HTTPConnection(host, 80, timeout=30)
    acme.request('GET', '/.well-known/acme-challenge/token', headers={'Host': host})
    answer = acme.getresponse()
    text = answer.read(4096)
    redirect = http.client.HTTPConnection(host, 80, timeout=30)
    redirect.request('GET', '/other', headers={'Host': host})
    other = redirect.getresponse()
    other.read()
    cell(s, 'F8-12 ACME validation on port 80 gets an explanation, any other request the https redirect',
         answer.status == 404 and answer.getheader('X-Paperboat-Error') == 'acme_behind_tunnel' and len(text) > 40 and other.status in (301, 308),
         '%s %s / %s' % (answer.status, text[:60], other.status))
    origin.stop()


def keepalive_scenario():
    s = 'keepalive'
    origin = serve(KeepAliveOrigin)
    name, host, route = create(s, '--port', str(origin.server_address[1]))
    failures, count, ip = [], 0, serving_ip(host)
    for index in range(120):  # requests at the origin's idle limit, +-0.15 s: a pooled connection the origin just closed must not fail one
        status = status_via(host, ip)
        count += 1
        if status != 200:
            failures.append((index, status))
        time.sleep(0.85 + (index % 4) * 0.1)
    cell(s, 'F8-09 Node-style keep-alive never races a pooled connection (%d requests at the origin\'s idle limit)' % count, not failures, failures[:5] or 'no failures')
    origin.stop()


def ws_scenario():
    s = 'ws'
    origin = serve(Origin)
    name, host, route = create(s, '--port', str(origin.server_address[1]))
    visit(host, '/')  # warm the route
    for attempt in range(15):  # DNS gives both edges: a connection that lands on an edge not yet serving the route is tried again
        sock = socket.create_connection((host, 443), timeout=30)
        tls = ssl.create_default_context().wrap_socket(sock, server_hostname=host)
        key = base64.b64encode(os.urandom(16)).decode()
        tls.sendall(('GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n' % (host, key)).encode())
        reply = b''
        while b'\r\n\r\n' not in reply:
            chunk = tls.recv(4096)
            if not chunk:
                break
            reply += chunk
        if reply.startswith(b'HTTP/1.1 101'):
            break
        tls.close()
        time.sleep(4)
    stream = tls.makefile('rb')
    tls.sendall(frame_bytes(1, b'first', mask=True))
    first = read_frame(stream)
    # Quiet means no frame from this side: wait in 20 s steps and notice at once if the connection ends, and when.
    started, closed_at, buffer = time.time(), None, b''
    stream.close()
    while time.time() - started < ARGS.ws_quiet and closed_at is None:
        tls.settimeout(20)
        try:
            data = tls.recv(4096)
            if not data:
                closed_at = time.time() - started
            buffer += data  # a ping would need a pong; none is expected from a quiet origin
        except socket.timeout:
            pass
        except OSError:
            closed_at = time.time() - started
    late = None
    if closed_at is None:
        tls.settimeout(60)
        tls.sendall(frame_bytes(1, b'late', mask=True))
        stream = tls.makefile('rb')
        try:
            late = read_frame(stream)
        except OSError as exc:
            late = ('error', str(exc))
    cell(s, 'F8-09 a WebSocket stays open through %d s of quiet' % ARGS.ws_quiet,
         reply.startswith(b'HTTP/1.1 101') and first == (1, b'first') and late == (1, b'late'),
         '%s first=%s late=%s closed_after=%s s' % (reply[:20], first, late, closed_at and round(closed_at)))
    tls.close()
    origin.stop()


def doctor_hint(name, words):
    """`pb tunnel doctor` (text) after the owner's own request made the origin state origin_tls: the hint names the fix."""
    deadline = time.time() + 90
    text = ''
    while time.time() < deadline:
        text = pb('tunnel', 'doctor', name, text=True)
        if all(word in text for word in words):
            return True, text
        time.sleep(6)
    return False, text


def tls_scenario():
    s = 'tls'
    selfp, selfk, selfc, _ = make_cert('app.local', ['localhost'])
    https = serve(Origin, tls=(write('self.crt', selfp), write('self.key', selfk)))
    port = https.server_address[1]
    placeholder = serve(Origin)
    name, host, route = create(s, '--port', str(placeholder.server_address[1]))  # the route moves to each https origin below
    target = 'https://127.0.0.1:%d' % port
    move(name, route, target, '--tls-verification', 'insecure')
    status, response, body = visit(host, '/')
    seen = json.loads(body) if status == 200 else {}
    cell(s, 'F8-10 self-signed loopback origin works with --tls-verification insecure, and it was sent the public name as SNI',
         status == 200 and seen.get('sni') == [host], '%s %s' % (status, body[:120]))
    move(name, route, target, '--tls-verification', 'pinned', '--tls-pin', fingerprint(selfc))
    status, response, body = visit(host, '/')
    cell(s, 'F8-10 --tls-verification pinned --tls-pin trusts exactly that certificate', status == 200, '%s %s' % (status, response.getheader('X-Paperboat-Error')))
    move(name, route, target, '--tls-verification', 'pinned', '--tls-pin', 'sha256:' + '0' * 64)
    status, response, body = visit(host, '/')
    cell(s, 'F8-10/F8-12 a changed pin is refused with the origin_certificate_changed page', status == 502 and response.getheader('X-Paperboat-Error') == 'origin_certificate_changed',
         '%s %s' % (status, response.getheader('X-Paperboat-Error')))
    move(name, route, target, '--tls-verification', 'system')
    status, response, body = visit(host, '/')
    ok, text = doctor_hint(name, ['self-signed', '--tls-verification pinned'])
    cell(s, 'F8-10/F8-12 a self-signed certificate: origin_tls page and the doctor hint names pinning', status == 502 and response.getheader('X-Paperboat-Error') == 'origin_tls' and ok,
         '%s %s %s' % (status, response.getheader('X-Paperboat-Error'), text[-200:]))
    https.stop()
    # the CA-signed leaf: the loopback origin is dialled with the public name, so the leaf names it
    ca_pem, ca_key, ca_cert, ca_priv = make_cert('Tor Test CA', ca=True, valid=(-1, 3))
    leaf_pem, leaf_key, _, _ = make_cert(host, [host], issuer=(ca_cert, ca_priv))
    signed = serve(Origin, tls=(write('leaf.crt', leaf_pem + ca_pem), write('leaf.key', leaf_key)))
    target = 'https://127.0.0.1:%d' % signed.server_address[1]
    move(name, route, target)
    status, response, body = visit(host, '/')
    ok, text = doctor_hint(name, ['signed by', '--tls-ca'])
    cell(s, 'F8-10/F8-12 a private-CA certificate without --tls-ca: origin_tls page and the doctor hint names --tls-ca',
         status == 502 and response.getheader('X-Paperboat-Error') == 'origin_tls' and ok, '%s %s %s' % (status, response.getheader('X-Paperboat-Error'), text[-200:]))
    move(name, route, target, '--tls-ca', write('ca.pem', ca_pem))
    status, response, body = visit(host, '/')
    cell(s, 'F8-10 a private CA (Caddy tls internal style) works with --tls-ca', status == 200, '%s %s' % (status, response.getheader('X-Paperboat-Error')))
    signed.stop()
    expired_pem, expired_key, _, _ = make_cert(host, [host], issuer=(ca_cert, ca_priv), valid=(-30, -1))
    old = serve(Origin, tls=(write('old.crt', expired_pem + ca_pem), write('old.key', expired_key)))
    move(name, route, 'https://127.0.0.1:%d' % old.server_address[1], '--tls-ca', write('ca2.pem', ca_pem))
    status, response, body = visit(host, '/')
    ok, text = doctor_hint(name, ['expired'])
    cell(s, 'F8-10/F8-12 an expired certificate: origin_tls page and the doctor hint says expired', status == 502 and response.getheader('X-Paperboat-Error') == 'origin_tls' and ok,
         '%s %s %s' % (status, response.getheader('X-Paperboat-Error'), text[-200:]))
    old.stop()


def grpc_server(port, credentials=None):
    server = grpc.server(__import__('concurrent.futures', fromlist=['x']).ThreadPoolExecutor(max_workers=8))
    servicer = health.HealthServicer()
    servicer.set('', health_pb2.HealthCheckResponse.SERVING)
    servicer.set('up', health_pb2.HealthCheckResponse.SERVING)  # never flips: the unary call's service
    health_pb2_grpc.add_HealthServicer_to_server(servicer, server)
    bound = server.add_secure_port('127.0.0.1:%d' % port, credentials) if credentials else server.add_insecure_port('127.0.0.1:%d' % port)
    server.start()

    def flip():
        while True:
            time.sleep(1.5)
            servicer.set('', health_pb2.HealthCheckResponse.NOT_SERVING)
            time.sleep(1.5)
            servicer.set('', health_pb2.HealthCheckResponse.SERVING)
    threading.Thread(target=flip, daemon=True).start()
    return server, bound


def grpc_probe(s, label, host):
    channel = grpc.secure_channel(host + ':443', grpc.ssl_channel_credentials())
    stub = health_pb2_grpc.HealthStub(channel)
    result = {}
    try:
        result['unary'] = health_pb2.HealthCheckResponse.ServingStatus.Name(stub.Check(health_pb2.HealthCheckRequest(service='up'), timeout=30).status)
        try:
            stub.Check(health_pb2.HealthCheckRequest(service='missing'), timeout=30)
        except grpc.RpcError as error:  # the status travels in the trailers: grpc-status 5 reaches the client as NOT_FOUND
            result['trailer'] = error.code().name
        seen = []
        for update in stub.Watch(health_pb2.HealthCheckRequest(service=''), timeout=30):
            seen.append(health_pb2.HealthCheckResponse.ServingStatus.Name(update.status))
            if len(seen) >= 2 and seen[-1] != seen[0]:
                break
        result['stream'] = seen
    except grpc.RpcError as error:
        result['error'] = '%s %s' % (error.code().name, (error.details() or '')[:150])
        result['edge_error'] = visit(host, '/', retries=1)[1].getheader('X-Paperboat-Error')  # what the edge says about this route
    cell(s, 'F8-11 gRPC over %s: unary, streaming and the grpc-status trailer arrive' % label,
         result.get('unary') == 'SERVING' and result.get('trailer') == 'NOT_FOUND' and len(result.get('stream', [])) >= 2 and result['stream'][0] != result['stream'][-1], result)
    channel.close()


def grpc_scenario():
    s = 'grpc'
    h2c, port = grpc_server(0)
    name, host, route = create(s, '--from', 'h2c://127.0.0.1:%d' % port)
    time.sleep(4)
    grpc_probe(s, 'an h2c origin', host)
    ca_pem, ca_key, ca_cert, ca_priv = make_cert('Tor gRPC CA', ca=True, valid=(-1, 3))
    leaf_pem, leaf_key, _, _ = make_cert(host, [host], issuer=(ca_cert, ca_priv))
    secure, tls_port = grpc_server(0, grpc.ssl_server_credentials([(leaf_key, leaf_pem + ca_pem)]))
    move(name, route, 'https://127.0.0.1:%d' % tls_port, '--tls-ca', write('grpc-ca.pem', ca_pem))
    grpc_probe(s, 'a TLS (h2) origin verified with --tls-ca', host)
    h2c.stop(0)
    secure.stop(0)


def failures_scenario():
    s = 'failures'
    origin = serve(Origin)
    name, host, route = create(s, '--port', str(origin.server_address[1]))
    visit(host, '/')
    origin.stop()  # the app dies while the tunnel is up
    status, response, body = visit(host, '/', retries=1)
    cell(s, 'F8-12 an app that is not running: the origin_refused page says so', status == 502 and response.getheader('X-Paperboat-Error') in ('origin_refused', 'origin_unreachable') and len(body) > 200,
         '%s %s' % (status, response.getheader('X-Paperboat-Error')))
    slow = serve(Origin)
    move(name, route, 'http://127.0.0.1:%d' % slow.server_address[1])
    visit(host, '/')  # the new route is known to the edge before the clock starts
    started = time.time()
    status, response, body = visit(host, '/slow?seconds=%d' % ARGS.slow, timeout=ARGS.slow + 60, retries=1)
    cell(s, 'F8-12 a slow app gets 100 s to its first byte, then the 504 origin_timeout page',
         status == 504 and response.getheader('X-Paperboat-Error') == 'origin_timeout' and 95 <= time.time() - started <= ARGS.slow + 20,
         '%s %s after %d s' % (status, response.getheader('X-Paperboat-Error'), time.time() - started))
    slow.stop()


def edges_scenario():
    """F8-15: every edge of the pair serves a new tunnel and a new preview within BOTH_EDGES seconds of the CLI returning."""
    s = 'edges'
    origin = serve(Origin)
    port = origin.server_address[1]
    name, host, route = create(s, '--port', str(port))
    preview = pb('preview', 'http://127.0.0.1:%d' % port, '--background', '--ttl', '1h', timeout=240)
    try:
        for kind, public in (('tunnel', host), ('preview', preview['endpoint'].split('://', 1)[1].rstrip('/'))):
            started, first = time.time(), {}
            ips = edge_ips(public)
            while time.time() - started < BOTH_EDGES and len(first) < len(ips):
                for ip in ips:
                    if ip not in first and status_via(public, ip) == 200:
                        first[ip] = round(time.time() - started, 1)
                time.sleep(1)
            cell(s, 'F8-15 a new %s is served by every edge (%d) within %d s' % (kind, len(ips), BOTH_EDGES), len(ips) >= 2 and len(first) == len(ips), 'seconds to first 200 per edge: %s of %s' % (first, ips))
    finally:
        pb('preview', 'stop', preview['id'], check=False)
        origin.stop()


def edge_loss_scenario():
    """F8-15 (Linux, needs sudo nft): one edge becomes unreachable from the machine, as a dead edge is; the other edge keeps serving
    a tunnel and a preview without a single failed request, and the lost edge serves again once it is reachable."""
    s = 'edge_loss'
    origin = serve(Origin)
    port = origin.server_address[1]
    name, host, route = create(s, '--port', str(port))
    preview = pb('preview', 'http://127.0.0.1:%d' % port, '--background', '--ttl', '1h', timeout=240)
    public = {'tunnel': host, 'preview': preview['endpoint'].split('://', 1)[1].rstrip('/')}
    blocked = None
    try:
        def serve_on(kind, ip, seconds):
            deadline = time.time() + seconds
            while time.time() < deadline:
                if status_via(public[kind], ip) == 200:
                    return True
                time.sleep(2)
            return False
        ips = edge_ips(host)
        both = all(serve_on(kind, ip, 90) for kind in public for ip in ips)
        cell(s, 'F8-15 before the loss every edge serves the tunnel and the preview', both and len(ips) >= 2, ips)
        lost, survivor = ips[0], ips[1]
        subprocess.run(['sudo', '-n', 'nft', '-f', '-'], input='table inet torblock { chain out { type filter hook output priority 0; ip daddr %s drop; } }\n' % lost, text=True, check=True)
        blocked = lost
        failures, deadline = [], time.time() + 75
        while time.time() < deadline:
            for kind in public:
                status = status_via(public[kind], survivor)
                if status != 200:
                    failures.append((round(time.time() - deadline + 75), kind, status))
            time.sleep(2)
        cell(s, 'F8-15 with one edge lost the other keeps serving the tunnel and the preview for 75 s without a failed request', not failures, failures[:6] or 'no failures')
        subprocess.run(['sudo', '-n', 'nft', 'delete', 'table', 'inet', 'torblock'], check=True)
        blocked = None
        cell(s, 'F8-15 the edge serves both again within 90 s of being reachable', all(serve_on(kind, lost, 90) for kind in public), lost)
    finally:
        if blocked:
            subprocess.run(['sudo', '-n', 'nft', 'delete', 'table', 'inet', 'torblock'], check=False)
        pb('preview', 'stop', preview['id'], check=False)
        origin.stop()


def preview_scenario():
    s = 'preview'
    origin = serve(Origin)
    selfp, selfk, _, _ = make_cert('app.local', ['localhost'])
    https = serve(Origin, tls=(write('prev.crt', selfp), write('prev.key', selfk)))
    previews = []

    def publish(target):
        data = pb('preview', target, '--background', '--ttl', '3h', timeout=240)
        previews.append(data['id'])
        return data['endpoint'].split('://', 1)[1].rstrip('/')
    try:
        host = publish('http://127.0.0.1:%d' % origin.server_address[1])
        status, response, body = visit(host, '/', headers=SPOOF)
        seen = json.loads(body) if status == 200 else {}
        cell(s, 'F7-08 preview: forwarded headers, spoofs dropped, https, public host, one client IP',
             status == 200 and seen.get('xfp') == 'https' and host in str(seen.get('xfh')) and 'evil.example.test' not in str(seen.get('xfh')) + str(seen.get('host'))
             and '203.0.113.9' not in str(seen.get('xff')) + str(seen.get('forwarded')) and ',' not in str(seen.get('xff')), '%s %s' % (status, seen))
        status, response, body = visit(host, '/force')
        cell(s, 'F7-08 preview: an app that redirects to its own https URL gets the loop page naming the fix',
             status == 502 and response.getheader('X-Paperboat-Error') == 'origin_redirect_loop' and b'HTTP-to-HTTPS redirect' in body, '%s %s' % (status, response.getheader('X-Paperboat-Error')))
        started = time.time()
        status, response, body = visit(host, '/slow?seconds=%d' % ARGS.slow, timeout=ARGS.slow + 60, retries=1)
        cell(s, 'F7-08 preview: a slow app gets 100 s to its first byte, then the 504 origin_timeout page',
             status == 504 and response.getheader('X-Paperboat-Error') == 'origin_timeout' and 95 <= time.time() - started <= ARGS.slow + 20,
             '%s %s after %d s' % (status, response.getheader('X-Paperboat-Error'), time.time() - started))
        secure_host = publish('https://127.0.0.1:%d' % https.server_address[1])
        status, response, _ = visit(secure_host, '/', retries=1)
        cell(s, 'F7-08 preview: an HTTPS origin on localhost is sent the public hostname as SNI (its self-signed certificate is then refused with origin_tls)',
             Origin.sni and Origin.sni[-1] == secure_host and status == 502 and response.getheader('X-Paperboat-Error') == 'origin_tls', '%s %s sni=%s' % (status, response.getheader('X-Paperboat-Error'), Origin.sni[-1:]))
    finally:
        for identity in previews:
            pb('preview', 'stop', identity, check=False)
        origin.stop()
        https.stop()


SCENARIOS = {'headers': headers_scenario, 'keepalive': keepalive_scenario, 'ws': ws_scenario, 'tls': tls_scenario, 'grpc': grpc_scenario, 'failures': failures_scenario, 'preview': preview_scenario, 'edges': edges_scenario, 'edge_loss': edge_loss_scenario}


def run_scenario(name):
    try:
        SCENARIOS[name]()
    except Exception as exc:
        cell(name, 'scenario ran to the end', False, '%s: %s' % (type(exc).__name__, exc))


def main():
    global ARGS
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--pb', required=True)
    parser.add_argument('--out', required=True)
    parser.add_argument('--only', nargs='*', default=[n for n in SCENARIOS if n != 'edge_loss'], help='scenarios to run (edge_loss, Linux with sudo, only when asked)')
    parser.add_argument('--ws-quiet', type=int, default=600)
    parser.add_argument('--slow', type=int, default=105)
    ARGS = parser.parse_args()
    threads = [threading.Thread(target=run_scenario, args=(name,)) for name in ARGS.only]
    try:
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()
    finally:
        delete_all()
        with open(ARGS.out, 'w') as out:
            json.dump({'check': 'tunnel-origin-rows', 'platform': sys.platform, 'cells': CELLS, 'retried_requests': LATE}, out, indent=1)
    sys.exit(1 if any(c['result'] != 'ok' for c in CELLS) else 0)


if __name__ == '__main__':
    main()
