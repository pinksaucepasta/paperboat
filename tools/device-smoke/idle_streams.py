#!/usr/bin/env python3
"""The test origin and the stream holder of staging_edge_idle.py (F21-37). Standard library only and no import from this
directory: the row also runs this file inside the staging fixtures.

idle_streams.py origin DIR ECHO_PORT TLS_PORT HTTP_PORT MARKER [LIFETIME_SECONDS]
    On 127.0.0.1: a TCP echo, a TLS echo with DIR/tls.crt and DIR/tls.key, and an HTTP server answering 200 with MARKER.
    Neither echo ever closes a connection that its client keeps open. The process ends by itself after LIFETIME_SECONDS.
idle_streams.py hold JSON
    {"streams": [{"name", "host", "port", "busy", "sni"?, "pin"?}], "hold": seconds, "every": seconds, "open": seconds}
    Opens every stream at once (retrying for "open" seconds until one line echoes), then reads each one: a quiet stream
    sends nothing more, a busy one sends one byte every "every" seconds. Prints one JSON line: a result per stream.
"""
import datetime
import hashlib
import http.server
import json
import os
import signal
import socket
import socketserver
import ssl
import sys
import threading
import time

ECHO_WAIT = 15  # a busy stream whose byte does not come back within this is not alive


def utc():
    return datetime.datetime.now(datetime.timezone.utc).isoformat(timespec='milliseconds')


class Echo(socketserver.BaseRequestHandler):
    def handle(self):
        connection = self.request
        try:
            if self.server.tls:
                connection = self.server.tls.wrap_socket(connection, server_side=True)
            while True:
                data = connection.recv(4096)
                if not data:
                    return
                connection.sendall(data)
        except OSError:
            return


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True
    tls = None


def origin(directory, echo_port, tls_port, http_port, marker, lifetime=900):
    class Page(http.server.BaseHTTPRequestHandler):
        def do_GET(self):  # noqa: N802 the handler's name
            body = marker.encode()
            self.send_response(200)
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *args):
            pass

    echo = Server(('127.0.0.1', int(echo_port)), Echo)
    secure = Server(('127.0.0.1', int(tls_port)), Echo)
    secure.tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    secure.tls.load_cert_chain(os.path.join(directory, 'tls.crt'), os.path.join(directory, 'tls.key'))
    page = http.server.ThreadingHTTPServer(('127.0.0.1', int(http_port)), Page)
    for server in (echo, secure, page):
        threading.Thread(target=server.serve_forever, daemon=True).start()
    signal.alarm(int(lifetime))  # a run that died leaves no origin behind
    threading.Event().wait()


def connect(spec, timeout=10):
    """A connected socket; with "sni", TLS to the origin, proved by the SHA-256 of the certificate it presents."""
    connection = socket.create_connection((spec['host'], spec['port']), timeout)
    if not spec.get('sni'):
        return connection
    try:
        context = ssl.create_default_context()
        context.check_hostname, context.verify_mode = False, ssl.CERT_NONE  # the pin below is the check
        connection = context.wrap_socket(connection, server_hostname=spec['sni'])
        seen = hashlib.sha256(connection.getpeercert(binary_form=True)).hexdigest()
        if seen != spec['pin']:
            raise OSError('not the origin certificate through the edge (sha256 %s): TLS did not pass through' % seen[:16])
    except BaseException:
        connection.close()
        raise
    return connection


def read_exactly(connection, wanted, timeout):
    connection.settimeout(timeout)
    got = b''
    while len(got) < len(wanted):
        data = connection.recv(len(wanted) - len(got))
        if not data:
            raise OSError('closed before the echo')
        got += data
    if got != wanted:
        raise OSError('the echo differs')


def open_stream(spec, seconds, dial=connect, sleep=time.sleep, clock=time.monotonic):
    """A stream that already carried one line to the origin and back. Retries while the route is not served yet."""
    deadline, attempts = clock() + seconds, 0
    while True:
        attempts += 1
        connection = None
        try:
            connection = dial(spec)
            line = b'open %s\n' % spec['name'].encode()
            connection.sendall(line)
            read_exactly(connection, line, 15)
            return connection, attempts
        except OSError as error:
            if connection is not None:
                connection.close()
            if clock() >= deadline:
                raise OSError('not opened in %d attempts over %d s: %s' % (attempts, seconds, str(error)[:160])) from error
            sleep(3)


def watch(connection, busy, hold, every, clock=time.monotonic):
    """Reads the open stream for `hold` seconds. closed: None (still alive), 'eof' (the peer's FIN), 'rst', or what failed.
    after: seconds from the last data to the close. A busy stream is alive only if its last byte also came back."""
    began = last = clock()
    result = {'closed': None, 'sent': 0, 'echoed': 0}
    pending, due = None, began + every  # pending: when the unanswered byte went out

    def ended(how):
        result.update(closed=how, closed_utc=utc(), after=round(clock() - last, 1), age=round(clock() - began, 1))
        return result

    try:
        while True:
            now = clock()
            over = now - began >= hold
            if busy and pending is None and (now >= due or over):
                connection.sendall(b'.')
                result['sent'] += 1
                pending, due = now, due + every
            elif over and pending is None:
                break
            if pending is not None and now - pending > ECHO_WAIT:
                return ended('stalled: a byte was not echoed within %d s' % ECHO_WAIT)
            connection.settimeout(0.2)
            try:
                data = connection.recv(4096)
            except TimeoutError:  # socket.timeout
                continue
            if not data:
                return ended('eof')
            last = clock()
            if pending is not None:
                result['echoed'] += 1
                pending = None
                if over:
                    break
    except ConnectionResetError:
        return ended('rst')
    except OSError as error:
        return ended('error: %s' % str(error)[:160])
    result.update(age=round(clock() - began, 1), alive_utc=utc())
    return result


def hold_one(spec, hold, every, open_seconds):
    result = {'name': spec['name'], 'busy': bool(spec['busy'])}
    try:
        connection, result['attempts'] = open_stream(spec, open_seconds)
    except OSError as error:
        return dict(result, opened=False, error=str(error)[:300])
    result.update(opened=True, opened_utc=utc())
    try:
        result.update(watch(connection, spec['busy'], hold, every))
    finally:
        connection.close()
    return result


def hold_all(plan, one=hold_one):
    results = {}

    def work(spec):
        try:
            results[spec['name']] = one(spec, plan['hold'], plan['every'], plan['open'])
        except Exception as error:  # noqa: BLE001 every stream has a result
            results[spec['name']] = {'name': spec['name'], 'busy': bool(spec['busy']), 'opened': False, 'error': str(error)[:300]}

    threads = [threading.Thread(target=work, args=(spec,), daemon=True) for spec in plan['streams']]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    return [results[spec['name']] for spec in plan['streams']]


def main(argv):
    if len(argv) >= 7 and argv[1] == 'origin':
        origin(*argv[2:])
    elif len(argv) == 3 and argv[1] == 'hold':
        print(json.dumps({'streams': hold_all(json.loads(argv[2]))}), flush=True)
    else:
        print(__doc__, file=sys.stderr)
        return 2
    return 0


if __name__ == '__main__':
    raise SystemExit(main(sys.argv))
