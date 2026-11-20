#!/usr/bin/env python3
"""Region drain check, run right after the region-pair deploy: two Paperboat edge nodes in ONE region serve one preview,
and the operator's Drain and Drain-and-move do what they promise. Prints one JSON receipt; exit 0 only if every step passes.

Before running (operator steps, done once):
  * both nodes are enrolled into the same region (admin panel, Infrastructure), ready, public ingress verified;
  * a workspace whose edge pair names that region has a paired machine running `pb preview <port>` for a local server that
    answers a known marker; pass its URL as --preview-url and the marker as --marker;
  * --node NODE_ID=PUBLIC_IP twice (the operator API's node id and that node's public ingress address);
  * --cred: the operator credential file (line 1 email, line 2 password, line 3 base32 key of the authenticator), never printed.
Steps (each asserted; a failing step stops the run, and the node is resumed on the way out):
  serve-all  every node serves the preview when the request is pinned to its address (curl --resolve): the machine holds a
             carrier to every node of the region
  dns-all    (--check-dns, needs PAPERBOAT_CERTIFICATES_MANAGE_EDGE_WILDCARDS) the preview host resolves to every node address
  drain      Drain node A: it shows `draining`; a connection opened to A before keeps working (existing ones finish); the
             other node keeps serving; with --check-dns A's address leaves DNS; unpinned requests never fail
  resume     Resume A: it is ready again and serves the preview again (the machine re-holds its carrier)
  move       Drain and move node A: the connection opened to A before ends within --move-seconds (it moved), the other node
             keeps serving, unpinned requests never fail, and the operator's `carriers` count for A drops to 0
  resume     Resume A again, as above
"""
import argparse
import base64
import hashlib
import hmac
import http.client
import http.cookiejar
import json
import socket
import ssl
import sys
import threading
import time
import urllib.request
import uuid
from pathlib import Path
from urllib.parse import urlsplit


class Fail(Exception):
    """One step failed: the step name and what was seen."""


def totp(secret, offset=0, now=None):
    """The RFC 6238 code (30 s steps, 6 digits, SHA-1) an authenticator shows for a base32 secret."""
    secret = secret.replace(' ', '').upper()
    step = int((time.time() if now is None else now) // 30) + offset
    key = base64.b32decode(secret + '=' * (-len(secret) % 8))
    mac = hmac.new(key, step.to_bytes(8, 'big'), hashlib.sha1).digest()
    at = mac[-1] & 15
    return '%06d' % ((int.from_bytes(mac[at:at + 4], 'big') & 0x7fffffff) % 1000000)


class Operator:
    """The operator API over HTTPS with the operator's session cookie and CSRF token; the password is never printed."""

    def __init__(self, api, cred, cafile=None):
        self.api, self.cred = api.rstrip('/'), Path(cred)
        context = ssl.create_default_context(cafile=cafile) if cafile else ssl.create_default_context()
        self.opener = urllib.request.build_opener(
            urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()), urllib.request.HTTPSHandler(context=context))
        self.csrf = ''

    def call(self, method, path, body=None, target=''):
        headers = {'Content-Type': 'application/json'}
        if method != 'GET':
            headers.update({'X-CSRF-Token': self.csrf, 'Idempotency-Key': uuid.uuid4().hex,
                            'Operator-Target': target or 'region-drain-check', 'Operator-Reason': 'region drain check'})
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

    def node(self, node_id, region=''):
        """The node's operator record ({state, ready, revision, carriers, ...}); region narrows the listing."""
        after = ''
        while True:
            status, data = self.call('GET', '/v1/admin/nodes?limit=100%s%s' % (
                '&region=' + region if region else '', '&after=' + after if after else ''))
            if status != 200:
                raise Fail('listing nodes failed (HTTP %s)' % status)
            for item in data['data']['items']:
                if item['id'] == node_id:
                    return item
            after = data['data'].get('next_after')
            if not after:
                raise Fail('node %s is not in the operator list' % node_id)

    def act(self, node_id, action):
        record = self.node(node_id)
        status, _ = self.call('POST', '/v1/admin/nodes/%s/%s' % (node_id, action), {'revision': record['revision']}, target=node_id)
        if status != 200:
            raise Fail('%s %s failed (HTTP %s)' % (action, node_id, status))


class PinnedConnection(http.client.HTTPSConnection):
    """An HTTPS connection to one address with the right server name (curl --resolve) and a keep-alive socket we reuse.

    auto_open is off once connected, so a socket the far side closed raises instead of quietly reconnecting: a cut
    connection must not look like a connection that kept working.
    """

    def __init__(self, host, ip, path, port=443, timeout=15):
        super().__init__(host, port, timeout=timeout, context=ssl.create_default_context())
        self.ip, self.path = ip, path

    def connect(self):
        sock = socket.create_connection((self.ip, self.port), self.timeout)
        self.sock = self._context.wrap_socket(sock, server_hostname=self.host)

    def hold(self):
        self.connect()
        self.auto_open = 0
        return self

    def body(self):
        self.request('GET', self.path, headers={'Connection': 'keep-alive'})
        return self.getresponse().read().decode('utf-8', 'replace')


def wait_until(check, seconds, step=2, sleep=time.sleep, now=time.monotonic):
    """Seconds until check() is true, or None after `seconds`."""
    started = now()
    while True:
        if check():
            return round(now() - started, 1)
        if now() - started > seconds:
            return None
        sleep(step)


class Probe(threading.Thread):
    """Unpinned requests once a second through the normal resolver: what a visitor sees while nodes come and go."""

    def __init__(self, fetch, marker):
        super().__init__(daemon=True)
        self.fetch, self.marker, self.failures, self.total, self.stop = fetch, marker, [], 0, threading.Event()

    def run(self):
        while not self.stop.is_set():
            self.total += 1
            detail = ''
            try:
                if self.marker not in self.fetch():
                    detail = 'wrong body'
            except Exception as error:  # a failed visitor request is the datum
                detail = type(error).__name__
            if detail:
                self.failures.append((round(time.time()), detail))
            self.stop.wait(1)


def run(env):
    """The steps against env (injected collaborators, so the logic is unit tested without a network). Returns the receipt."""
    receipt = {'steps': {}}
    a, b = env.nodes[0], env.nodes[1]

    def step(name, seconds):
        receipt['steps'][name] = seconds

    def serves(node):
        try:
            return env.marker in env.pinned(node)
        except Exception:
            return False

    def need(condition, name, detail):
        if not condition:
            raise Fail('%s: %s' % (name, detail))

    def resume_and_serve(name):
        env.operator.act(a, 'resume')
        up = wait_until(lambda: env.operator.node(a)['ready'], env.up_seconds, sleep=env.sleep, now=env.now)
        need(up is not None, name, 'node %s is not ready again within %ss' % (a, env.up_seconds))
        back = wait_until(lambda: serves(a), env.up_seconds, sleep=env.sleep, now=env.now)
        need(back is not None, name, 'node %s does not serve the preview again within %ss' % (a, env.up_seconds))
        step(name, back)

    for node in env.nodes:
        took = wait_until(lambda: serves(node), env.serve_seconds, sleep=env.sleep, now=env.now)
        need(took is not None, 'serve-all', 'node %s does not serve the preview within %ss' % (node, env.serve_seconds))
        step('serve-all:' + node, took)
    if env.check_dns:
        need({env.ips[n] for n in env.nodes} <= env.resolve(), 'dns-all', 'the preview host does not resolve to every node address')
    env.probe.start()
    try:
        held = env.held(a)
        need(env.marker in held.body(), 'drain', 'the connection to %s does not serve before the drain' % a)
        env.operator.act(a, 'drain')
        need(wait_until(lambda: env.operator.node(a)['state'] == 'draining', 15, sleep=env.sleep, now=env.now) is not None,
             'drain', 'node %s does not show draining' % a)
        env.sleep(env.settle_seconds)
        try:
            alive = env.marker in held.body()
        except Exception:
            alive = False
        need(alive, 'drain', 'the connection opened before the drain was cut: existing connections must finish')
        need(serves(b), 'drain', 'node %s stopped serving' % b)
        if env.check_dns:
            gone = wait_until(lambda: env.ips[a] not in env.resolve(), env.dns_seconds, sleep=env.sleep, now=env.now)
            need(gone is not None, 'drain', 'the node address stays in DNS for more than %ss' % env.dns_seconds)
            step('dns-drop', gone)
        resume_and_serve('resume')
        held = env.held(a)
        need(env.marker in held.body(), 'move', 'the connection to %s does not serve before the move' % a)
        env.operator.act(a, 'drain-move')
        ended = wait_until(lambda: not _serves_on(held, env.marker), env.move_seconds, sleep=env.sleep, now=env.now)
        need(ended is not None, 'move', 'the connection to %s is still served %ss after Drain and move' % (a, env.move_seconds))
        step('move-connection-ended', ended)
        need(serves(b), 'move', 'node %s stopped serving' % b)
        empty = wait_until(lambda: env.operator.node(a).get('carriers', -1) == 0, env.move_seconds, sleep=env.sleep, now=env.now)
        need(empty is not None, 'move', 'node %s still lists carriers after %ss' % (a, env.move_seconds))
        step('move-carriers-zero', empty)
        resume_and_serve('resume-after-move')
    finally:
        env.probe.stop.set()
        try:
            if env.operator.node(a)['state'] == 'draining':
                env.operator.act(a, 'resume')
        except Exception:
            pass
    need(len(env.probe.failures) <= env.max_probe_failures, 'visitors',
         '%d of %d unpinned requests failed: %s' % (len(env.probe.failures), env.probe.total, env.probe.failures[:5]))
    receipt['probe'] = {'requests': env.probe.total, 'failures': len(env.probe.failures)}
    return receipt


def _serves_on(held, marker):
    try:
        return marker in held.body()
    except Exception:
        return False


class Env:
    def __init__(self, args, operator):
        host = urlsplit(args.preview_url)
        self.nodes, self.ips = [n.split('=')[0] for n in args.node], dict(n.split('=', 1) for n in args.node)
        self.marker, self.operator, self.check_dns = args.marker, operator, args.check_dns
        self.serve_seconds, self.up_seconds, self.move_seconds = args.serve_seconds, args.up_seconds, args.move_seconds
        self.dns_seconds, self.settle_seconds, self.max_probe_failures = args.dns_seconds, args.settle_seconds, args.max_probe_failures
        self.sleep, self.now = time.sleep, time.monotonic
        self.host, self.path = host.hostname, (host.path or '/') + ('?' + host.query if host.query else '')
        self.probe = Probe(lambda: urllib.request.urlopen(args.preview_url, timeout=10).read().decode('utf-8', 'replace'), args.marker)

    def pinned(self, node):
        connection = PinnedConnection(self.host, self.ips[node], self.path)
        try:
            return connection.body()
        finally:
            connection.close()

    def held(self, node):
        return PinnedConnection(self.host, self.ips[node], self.path).hold()

    def resolve(self):
        return {info[4][0] for info in socket.getaddrinfo(self.host, 443, proto=socket.IPPROTO_TCP)}


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--api', required=True, help='the server API base URL')
    parser.add_argument('--cred', required=True)
    parser.add_argument('--cafile')
    parser.add_argument('--preview-url', required=True)
    parser.add_argument('--marker', required=True)
    parser.add_argument('--node', action='append', required=True, metavar='NODE_ID=PUBLIC_IP', help='twice, both in one region')
    parser.add_argument('--check-dns', action='store_true')
    parser.add_argument('--serve-seconds', type=int, default=120)
    parser.add_argument('--up-seconds', type=int, default=240)
    parser.add_argument('--move-seconds', type=int, default=150)
    parser.add_argument('--dns-seconds', type=int, default=90)
    parser.add_argument('--settle-seconds', type=int, default=20)
    parser.add_argument('--max-probe-failures', type=int, default=0)
    args = parser.parse_args()
    if len(args.node) != 2 or any('=' not in n for n in args.node):
        parser.error('--node NODE_ID=PUBLIC_IP is required exactly twice')
    operator = Operator(args.api, args.cred, args.cafile)
    operator.login()
    try:
        receipt = run(Env(args, operator))
    except Fail as error:
        print(json.dumps({'ok': False, 'failed': str(error)}))
        return 1
    except Exception as error:  # an unexpected failure is still a failed check with a receipt, not a traceback
        print(json.dumps({'ok': False, 'failed': '%s: %s' % (type(error).__name__, error)}))
        return 1
    print(json.dumps({'ok': True, **receipt}))
    return 0


if __name__ == '__main__':
    sys.exit(main())
