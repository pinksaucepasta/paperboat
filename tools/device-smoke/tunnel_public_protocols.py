#!/usr/bin/env python3
"""Public raw TCP and TLS passthrough tunnels end to end on the shared engine path (F20-03).

Runs on an enrolled Linux machine. A tcp:// origin is published as a public TCP tunnel and echoed through its
public port. An HTTP tunnel's default route is replaced by a TLS passthrough route to a TLS origin with its own
self-signed certificate, and the certificate that comes back through the edge is that origin's. Both tunnels are
deleted on every exit path.
"""
import json
from pathlib import Path
import socket
import socketserver
import ssl
import subprocess
import tempfile
import threading
import time
import uuid

from install_lifecycle import registration
from pairing_lifecycle import confirmed_command, invoke


class Echo(socketserver.BaseRequestHandler):
    def handle(self):
        self.request.sendall(b'echo:' + self.request.recv(100))


def retry(action, seconds=60):
    deadline = time.time() + seconds
    while True:
        try:
            return action()
        except OSError:
            if time.time() > deadline:
                raise
            time.sleep(2)


def tls_origin(directory):
    cert, key = Path(directory) / 'origin.crt', Path(directory) / 'origin.key'
    subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', str(key), '-out', str(cert),
                    '-days', '2', '-subj', '/CN=smoke-tls-origin'], check=True, capture_output=True)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(cert, key)
    listener = socket.socket()
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    listener.bind(('127.0.0.1', 0))
    listener.listen(8)

    def serve():
        while True:
            try:
                connection, _ = listener.accept()
            except OSError:
                return
            try:
                with context.wrap_socket(connection, server_side=True) as secured:
                    secured.recv(100)
                    secured.sendall(b'tls-origin-ok')
            except (OSError, ssl.SSLError):
                pass

    threading.Thread(target=serve, daemon=True).start()
    return listener


def main():
    binary = Path.home() / '.local/bin/pb'
    before = registration()
    suffix = uuid.uuid4().hex[:12]
    created = []

    def call(*args):
        return invoke(binary, 'tunnel', *args, '--json', timeout=120)

    echo = socketserver.ThreadingTCPServer(('127.0.0.1', 0), Echo)
    echo.daemon_threads = True
    threading.Thread(target=echo.serve_forever, daemon=True).start()
    primary_error = None
    with tempfile.TemporaryDirectory(prefix='paperboat-public-protocols-') as directory:
        origin = tls_origin(directory)
        try:
            tcp = call('create', 'smoke-tcp-' + suffix, '--from', 'tcp://127.0.0.1:%d' % echo.server_address[1],
                       '--wait', '--timeout', '90s')
            created.append(tcp['tunnel']['id'])
            route = call('route', 'list', created[-1])['items'][0]
            assert route['protocol'] == 'tcp' and route['public_tcp_port'] >= 1024, route
            host = route['host_match']['hostname']

            def through_tcp():
                with socket.create_connection((host, route['public_tcp_port']), timeout=15) as connection:
                    connection.sendall(b'smoke')
                    return connection.recv(100)
            assert retry(through_tcp) == b'echo:smoke'

            tls = call('create', 'smoke-tls-' + suffix, '--port', str(origin.getsockname()[1]), '--wait', '--timeout', '90s')
            created.append(tls['tunnel']['id'])
            default = call('route', 'list', created[-1])['items'][0]
            confirmed_command(binary, 'tunnel', 'route', 'remove', created[-1], default['id'])
            call('route', 'add', created[-1], '--name', 'tls1', '--protocol', 'tls',
                 '--to', 'tcp://127.0.0.1:%d' % origin.getsockname()[1], '--wait', '--timeout', '90s')
            added = call('route', 'list', created[-1])['items']
            assert [r['protocol'] for r in added] == ['tls'] and added[0]['public_tcp_port'] == 443, added
            tls_host = added[0]['host_match']['hostname']
            client = ssl.create_default_context()
            client.check_hostname, client.verify_mode = False, ssl.CERT_NONE

            def through_tls():
                with socket.create_connection((tls_host, 443), timeout=15) as plain:
                    with client.wrap_socket(plain, server_hostname=tls_host) as secured:
                        secured.sendall(b'smoke')
                        certificate = ssl.DER_cert_to_PEM_cert(secured.getpeercert(binary_form=True))
                        return secured.recv(100), certificate
            payload, certificate = retry(through_tls)
            assert payload == b'tls-origin-ok', payload
            # The certificate is the origin's own: the edge passed the TLS session through untouched.
            subject = subprocess.run(['openssl', 'x509', '-noout', '-subject'], input=certificate, text=True,
                                     capture_output=True, check=True).stdout
            assert 'smoke-tls-origin' in subject, subject
        except BaseException as error:
            primary_error = error
            raise
        finally:
            origin.close()
            echo.shutdown()
            echo.server_close()
            for identifier in created:
                try:
                    confirmed_command(binary, 'tunnel', 'delete', identifier)
                except BaseException:
                    if primary_error is None:
                        raise
                    print(json.dumps({'check': 'tunnel-public-protocols', 'cleanup': 'failed', 'tunnel': identifier}), flush=True)
    assert registration() == before
    print(json.dumps({'check': 'tunnel-public-protocols', 'status': 'PASS', 'public_tcp_echo': True,
                      'tls_passthrough_origin_certificate': True, 'enrollment_preserved': True}))


if __name__ == '__main__':
    main()
