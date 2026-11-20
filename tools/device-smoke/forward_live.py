#!/usr/bin/env python3
"""FORWARD live check: an edge node that does not hold a publication's carrier forwards the visitor's TLS stream to the node
that does, and the managed nodes are meshed. Prints a PASS/FAIL/SKIP table; exit 0 only if nothing FAILed.

Everything about the fleet comes from arguments or environment (nothing is stored in the repository):
  --api / PB_API                    the server API base URL
  --cred / PB_OPERATOR_CRED         path of the operator credential file (line 1 email, line 2 password, line 3 base32
                                    authenticator key); the file's content is never printed
  --edge NODE_ID=IP (repeat, >= 2)  an edge node's operator id and the public address of its port 443
                                    (PB_FORWARD_EDGES: comma separated list of the same)
  --publication NAME=NODE_ID        a publication name and the edge node that holds its carrier (repeat; one per node is
                                    enough; PB_FORWARD_PUBLICATIONS: comma separated)
  --relay NODE_ID                   a relay node id for --check-links (repeat; PB_FORWARD_RELAYS: comma separated)
  --tcp-name NAME --tcp-port PORT   a raw TCP tunnel: NAME labels it and, through --publication NAME=NODE_ID, names its holder
                                    (PB_FORWARD_TCP_NAME, PB_FORWARD_TCP_PORT). The tunnel's origin must be an echo server
                                    (the operator sets one up): the check sends one line and expects it back.
  --terminal-host HOST              the runtime host of a machine's browser terminal (PB_FORWARD_TERMINAL_HOST)
  --terminal-subprotocols-file PATH one fresh single-use ticket per edge, one per line, line i for edge i in --edge order
                                    (PB_FORWARD_TERMINAL_SUBPROTOCOLS_FILE). A ticket lives 30 s: mint them just before the run.
  --terminal-origin URL            the Origin header sent with the terminal upgrade (needed with --terminal-host)
Checks:
  terminal-any-edge X  for every edge X: a browser-terminal upgrade to X:443 (SNI and Host = HOST, the certificate must
               validate for it) with the edge's ticket must answer 101, and the same upgrade without a ticket must be refused
               (4xx). Runs first, while the tickets are fresh. Not requested => SKIP
  pair X>Y     for every ordered pair of edge nodes with a publication held by Y: TLS to X:443 with SNI = that name; the
               certificate chain must validate for the NAME with the system trust store (Y terminated TLS, X only relayed
               bytes) and GET / must answer --status; then X.forward.forwarded and Y.forward.accepted must rise and
               Y.forward.forwarded must not (the stream was never forwarded twice)
  tcp NAME:PORT via X  for every edge X: dial X:PORT, send a probe line, the echo must come back; then X.forward.forwarded
               must rise, unless X is the holder of NAME (then it must not rise). No holder for NAME => SKIP
  --check-links  every edge node reports forward.links >= edge nodes - 1; every relay node reports forward.links >= 1
  relay-data-path  needs a way to pin a throwaway daemon to one relay node; `pb` has none (relay choice is automatic), so
               it is always printed as SKIP rather than passed
"""
import argparse
import base64
import hashlib
import hmac
import http.client
import http.cookiejar
import json
import os
import socket
import ssl
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

TCP_PROBE = b'paperboat-forward-probe\n'
UPGRADE_HEAD_LIMIT = 8192


class Fail(Exception):
    """A check could not run (login, listing): the run stops with this message."""


def totp(secret, offset=0, now=None):
    """The RFC 6238 code (30 s steps, 6 digits, SHA-1) an authenticator shows for a base32 secret."""
    secret = secret.replace(' ', '').upper()
    step = int((time.time() if now is None else now) // 30) + offset
    key = base64.b32decode(secret + '=' * (-len(secret) % 8))
    mac = hmac.new(key, step.to_bytes(8, 'big'), hashlib.sha1).digest()
    at = mac[-1] & 15
    return '%06d' % ((int.from_bytes(mac[at:at + 4], 'big') & 0x7fffffff) % 1000000)


class Operator:
    """The operator API over HTTPS with the operator's session cookie; the password is never printed."""

    def __init__(self, api, cred):
        self.api, self.cred, self.csrf = api.rstrip('/'), Path(cred), ''
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

    def call(self, method, path, body=None):
        headers = {'Content-Type': 'application/json'}
        if method != 'GET':
            headers['X-CSRF-Token'] = self.csrf
        request = urllib.request.Request(self.api + path, method=method, headers=headers,
                                         data=json.dumps(body).encode() if body is not None else None)
        try:
            with self.opener.open(request, timeout=30) as response:
                return response.status, json.loads(response.read() or b'null')
        except urllib.error.HTTPError as error:
            return error.code, None

    def login(self):
        lines = self.cred.read_text().split('\n')
        email, password, secret = lines[0], lines[1], (lines[2].strip() if len(lines) > 2 else '')
        status, data = self.call('POST', '/v1/operator/login', {'email': email, 'password': password})
        if status != 200:
            raise Fail('operator login failed (HTTP %s)' % status)
        self.csrf = data['data']['csrf_token']
        if data['data'].get('mfa') == 'verified':
            return
        if data['data'].get('mfa') != 'verify' or not secret:
            raise Fail('the operator needs a second factor: put the authenticator key on line 3 of the credential file')
        for offset in (0, 1):  # a code works once per 30 s step: the next step's code is accepted too
            status, _ = self.call('POST', '/v1/operator/mfa/verify', {'code': totp(secret, offset)})
            if status == 200:
                return
        raise Fail('the operator second factor was refused (HTTP %s)' % status)

    def forward_stats(self):
        """{node id: its forward counters, or None before it reported any} for every managed node."""
        stats, after = {}, ''
        while True:
            status, data = self.call('GET', '/v1/admin/nodes?limit=100' + ('&after=' + after if after else ''))
            if status != 200:
                raise Fail('listing nodes failed (HTTP %s)' % status)
            stats.update(parse_nodes(data))
            after = data['data'].get('next_after')
            if not after:
                return stats


def parse_nodes(page):
    """{node id: forward stats dict or None} from one GET /v1/admin/nodes response body."""
    return {item['id']: item.get('forward') for item in page['data']['items']}


def probe(ip, name, path='/', timeout=15):
    """GET path from ip:443 with SNI and Host = name. The default context validates the chain and the name against the
    system trust store, so a certificate that is not the holder's raises ssl.SSLCertVerificationError. Returns the status."""
    connection = http.client.HTTPSConnection(name, 443, timeout=timeout, context=ssl.create_default_context())
    try:
        connection.sock = connection._context.wrap_socket(socket.create_connection((ip, 443), timeout), server_hostname=name)
        connection.request('GET', path, headers={'Connection': 'close'})
        response = connection.getresponse()
        response.read(1 << 16)
        return response.status
    finally:
        connection.close()


def tcp_echo(ip, port, timeout=5):
    """True when the tunnel at ip:port echoes TCP_PROBE back, i.e. its origin is an echo server."""
    with socket.create_connection((ip, port), timeout) as sock:
        sock.sendall(TCP_PROBE)
        echoed = b''
        while len(echoed) < len(TCP_PROBE):
            chunk = sock.recv(len(TCP_PROBE) - len(echoed))
            if not chunk:
                break
            echoed += chunk
    return echoed == TCP_PROBE


def ws_status(ip, host, origin, subprotocol=None, timeout=10):
    """The status code answering a browser-terminal WebSocket upgrade to ip:443 with SNI and Host = host (the certificate
    must validate for host), or None when no status line came back. subprotocol is the edge's ticket, sent only when given."""
    lines = ['GET /v1/browser-terminal HTTP/1.1', 'Host: ' + host, 'Upgrade: websocket', 'Connection: Upgrade',
             'Sec-WebSocket-Version: 13', 'Sec-WebSocket-Key: ' + base64.b64encode(os.urandom(16)).decode(), 'Origin: ' + origin]
    if subprotocol is not None:
        lines.append('Sec-WebSocket-Protocol: ' + subprotocol)
    head = b''
    with socket.create_connection((ip, 443), timeout) as raw:
        with ssl.create_default_context().wrap_socket(raw, server_hostname=host) as tls:
            tls.sendall(('\r\n'.join(lines) + '\r\n\r\n').encode())
            while b'\r\n\r\n' not in head and len(head) < UPGRADE_HEAD_LIMIT:
                chunk = tls.recv(4096)
                if not chunk:
                    break
                head += chunk
    return status_of(head)


def status_of(head):
    """The status code of an HTTP response head, or None when its first line has none."""
    parts = head.split(b'\r\n', 1)[0].split(b' ')
    return int(parts[1]) if len(parts) > 1 and parts[1].isdigit() else None


def rose(before, after, node, key):
    """True when the node reported `key` and it grew; a node without counters never rose."""
    return bool(before.get(node) is not None and after.get(node) is not None and after[node][key] > before[node][key])


def pair_verdict(before, after, x, y):
    """(ok, detail) for one forwarded request X>Y from the counters read before and after it."""
    for node in (x, y):
        if before.get(node) is None or after.get(node) is None:
            return False, 'node %s reports no forward counters' % node
    problems = []
    if not rose(before, after, x, 'forwarded'):
        problems.append('%s.forwarded did not rise' % x)
    if not rose(before, after, y, 'accepted'):
        problems.append('%s.accepted did not rise' % y)
    if rose(before, after, y, 'forwarded'):
        problems.append('%s.forwarded rose: the stream was forwarded twice' % y)
    return not problems, '; '.join(problems) or 'forwarded %+d, accepted %+d' % (
        after[x]['forwarded'] - before[x]['forwarded'], after[y]['accepted'] - before[y]['accepted'])


def tcp_verdict(before, after, x, holder):
    """(ok, detail) for one raw TCP probe through edge x: x forwards to the holder unless it is the holder itself."""
    if before.get(x) is None or after.get(x) is None:
        return False, 'node %s reports no forward counters' % x
    if x == holder:
        if rose(before, after, x, 'forwarded'):
            return False, '%s.forwarded rose on the holder: forwarded twice' % x
        return True, 'the holder served the probe itself'
    if not rose(before, after, x, 'forwarded'):
        return False, '%s.forwarded did not rise' % x
    return True, 'forwarded %+d' % (after[x]['forwarded'] - before[x]['forwarded'])


def terminal_verdict(with_ticket, without_ticket):
    """(ok, detail) for one edge: the upgrade with its ticket must be 101, the same upgrade without it a 4xx refusal."""
    if with_ticket != 101:
        return False, 'the upgrade with its ticket answered %s, want 101' % (with_ticket or 'no status line')
    if without_ticket is None or not 400 <= without_ticket < 500:
        return False, 'the upgrade without a ticket answered %s, want a 4xx refusal' % (without_ticket or 'no status line')
    return True, '101 with the ticket, %s without' % without_ticket


def links_verdicts(stats, edges, relays):
    """[(check, ok, detail)] for --check-links from the nodes' forward.links."""
    rows = []
    for node, need in [(n, len(edges) - 1) for n in edges] + [(n, 1) for n in relays]:
        links = (stats.get(node) or {}).get('links')
        rows.append(('links %s' % node, links is not None and links >= need,
                     'no forward counters' if links is None else 'links=%d, need >= %d' % (links, need)))
    return rows


def settle(env, check):
    """check(stats) -> (ok, detail), re-read until it passes or the settle time is over (counters travel in heartbeats)."""
    deadline = env.now() + env.settle_seconds
    while True:
        ok, detail = check(env.stats())
        if ok or env.now() >= deadline:
            return ok, detail
        env.sleep(2)


def terminal_rows(env):
    """One row per edge (in --edge order): the upgrade with the edge's ticket must answer 101, without it a 4xx."""
    if not env.terminal_host:
        return [('terminal-any-edge', 'SKIP', 'not requested')]
    rows = []
    for i, (x, ip) in enumerate(env.edges.items()):
        row = 'terminal-any-edge %s' % x
        if i >= len(env.tickets) or not env.tickets[i]:
            rows.append((row, 'FAIL', 'no ticket on line %d of the subprotocols file (one line per edge, in --edge order)' % (i + 1)))
            continue
        try:
            with_ticket = env.ws_status(ip, env.terminal_host, env.terminal_origin, env.tickets[i])
            without = env.ws_status(ip, env.terminal_host, env.terminal_origin)
        except OSError as error:  # SSLCertVerificationError is an OSError: the certificate is not the runtime host's
            rows.append((row, 'FAIL', 'TLS/HTTP via %s for %s: %s' % (x, env.terminal_host, type(error).__name__)))
            continue
        ok, detail = terminal_verdict(with_ticket, without)
        rows.append((row, 'PASS' if ok else 'FAIL', detail))
    return rows


def tcp_rows(env):
    """One row per edge for the raw TCP probe on env.tcp_port; the holder is the node --publication names for env.tcp_name."""
    holder, rows = env.publications.get(env.tcp_name), []
    for x, ip in env.edges.items():
        row = 'tcp %s:%d via %s' % (env.tcp_name, env.tcp_port, x)
        if holder is None:
            rows.append((row, 'SKIP', 'holder unknown: no --publication %s=NODE_ID' % env.tcp_name))
            continue
        before = env.stats()
        try:
            echoed = env.tcp_echo(ip, env.tcp_port)
        except OSError as error:
            rows.append((row, 'FAIL', 'TCP via %s: %s' % (x, type(error).__name__)))
            continue
        if not echoed:
            rows.append((row, 'FAIL', 'the tunnel did not echo the probe line via %s' % x))
            continue
        ok, detail = settle(env, lambda after: tcp_verdict(before, after, x, holder))
        rows.append((row, 'PASS' if ok else 'FAIL', detail))
    return rows


def pair_rows(env):
    """One row per ordered pair of edges X>Y whose publication Y holds, checked through X."""
    rows = []
    for x in env.edges:
        for y in env.edges:
            if x == y:
                continue
            row = 'pair %s>%s' % (x, y)
            name = next((n for n, holder in env.publications.items() if holder == y), None)
            if name is None:
                rows.append((row, 'SKIP', 'no --publication whose carrier is held by %s' % y))
                continue
            before = env.stats()
            try:
                status = env.probe(env.edges[x], name)
            except (ssl.SSLError, OSError, http.client.HTTPException) as error:  # SSLCertVerificationError: not the holder's chain
                rows.append((row, 'FAIL', 'TLS/HTTP via %s for %s: %s' % (x, name, type(error).__name__)))
                continue
            if status != env.status:
                rows.append((row, 'FAIL', 'GET / for %s via %s answered HTTP %s, want %s' % (name, x, status, env.status)))
                continue
            ok, detail = settle(env, lambda after: pair_verdict(before, after, x, y))
            rows.append((row, 'PASS' if ok else 'FAIL', '%s: %s' % (name, detail)))
    if not any(r[1] in ('PASS', 'FAIL') for r in rows):
        rows.append(('pairs', 'FAIL', 'no ordered pair could be checked: give --edge twice and --publication for each holder'))
    return rows


def run(env):
    """The checks against env (injected collaborators, so the logic is unit tested without a network). Returns table rows."""
    rows = terminal_rows(env)  # first: the tickets live 30 s
    rows += pair_rows(env)
    if env.check_links:
        rows += [(c, 'PASS' if ok else 'FAIL', d) for c, ok, d in links_verdicts(env.stats(), list(env.edges), env.relays)]
    if env.tcp_port:
        rows += tcp_rows(env)
    rows.append(('relay-data-path', 'SKIP', 'pb has no way to pin a daemon to one relay node (relay choice is automatic)'))
    return rows


def table(rows):
    width = max(len(r[0]) for r in rows)
    return '\n'.join('%-4s  %-*s  %s' % (r[1], width, r[0], r[2]) for r in rows)


class Env:
    def __init__(self, args, operator):
        self.edges, self.publications = dict(args.edge), dict(args.publication)
        self.relays, self.check_links, self.status, self.settle_seconds = args.relay, args.check_links, args.status, args.settle_seconds
        self.tcp_name, self.tcp_port = args.tcp_name, args.tcp_port
        self.terminal_host, self.terminal_origin, self.tickets = args.terminal_host, args.terminal_origin, args.tickets
        self.stats, self.probe, self.sleep, self.now = operator.forward_stats, probe, time.sleep, time.monotonic
        self.tcp_echo, self.ws_status = tcp_echo, ws_status


def pairs(values, parser, flag):
    out = []
    for value in values:
        key, sep, rest = value.partition('=')
        if not sep or not key or not rest:
            parser.error('%s wants NAME=VALUE, got a malformed entry' % flag)
        out.append((key, rest))
    return out


def listed(name):
    return [v for v in os.environ.get(name, '').split(',') if v]


def read_tickets(path):
    """The ticket lines of the subprotocols file, one per edge. Tickets are single-use: they are never printed."""
    return [line.strip() for line in Path(path).read_text().splitlines()]


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--api', default=os.environ.get('PB_API'))
    parser.add_argument('--cred', default=os.environ.get('PB_OPERATOR_CRED'))
    parser.add_argument('--edge', action='append', default=listed('PB_FORWARD_EDGES'), metavar='NODE_ID=IP')
    parser.add_argument('--publication', action='append', default=listed('PB_FORWARD_PUBLICATIONS'), metavar='NAME=NODE_ID')
    parser.add_argument('--relay', action='append', default=listed('PB_FORWARD_RELAYS'), metavar='NODE_ID')
    parser.add_argument('--check-links', action='store_true')
    parser.add_argument('--status', type=int, default=200)
    parser.add_argument('--settle-seconds', type=int, default=90)
    parser.add_argument('--tcp-name', default=os.environ.get('PB_FORWARD_TCP_NAME'))
    parser.add_argument('--tcp-port', type=int, default=os.environ.get('PB_FORWARD_TCP_PORT'))
    parser.add_argument('--terminal-host', default=os.environ.get('PB_FORWARD_TERMINAL_HOST'))
    parser.add_argument('--terminal-subprotocols-file', default=os.environ.get('PB_FORWARD_TERMINAL_SUBPROTOCOLS_FILE'))
    parser.add_argument('--terminal-origin')
    args = parser.parse_args(argv)
    if not args.api or not args.cred or len(args.edge) < 2:
        parser.error('--api, --cred and --edge (at least twice) are required, as arguments or environment')
    if bool(args.tcp_name) != bool(args.tcp_port) or (args.tcp_port and not 0 < args.tcp_port < 65536):
        parser.error('--tcp-name and --tcp-port go together (the port is 1-65535)')
    terminal = (args.terminal_host, args.terminal_subprotocols_file, args.terminal_origin)
    if any(terminal) and not all(terminal):
        parser.error('--terminal-host, --terminal-subprotocols-file and --terminal-origin go together')
    args.edge, args.publication = pairs(args.edge, parser, '--edge'), pairs(args.publication, parser, '--publication')
    operator = Operator(args.api, args.cred)
    try:
        operator.login()
        args.tickets = read_tickets(args.terminal_subprotocols_file) if args.terminal_subprotocols_file else []
        rows = run(Env(args, operator))
    except Fail as error:
        print('FAIL  forward-live  %s' % error)
        return 1
    except Exception as error:  # an unexpected failure is still a failed check, not a traceback
        print('FAIL  forward-live  %s: %s' % (type(error).__name__, error))
        return 1
    print(table(rows))
    return 1 if any(r[1] == 'FAIL' for r in rows) else 0


if __name__ == '__main__':
    sys.exit(main())
