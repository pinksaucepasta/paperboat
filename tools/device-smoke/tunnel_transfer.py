#!/usr/bin/env python3
"""F8-06: 16 MiB upload and download through a durable tunnel arrive byte-exact."""
import hashlib
import http.server
import sys
import json
import os
from pathlib import Path
import threading
import urllib.request
import uuid

from install_lifecycle import registration
from tunnel_lifecycle import confirmed, run, wait

SIZE = 16 << 20
DOWNLOAD = os.urandom(SIZE)


class QuietServer(http.server.ThreadingHTTPServer):
    """A client that hangs up mid-response is not a failure of the check; only its own receipt is."""

    def handle_error(self, request, client_address):
        if not isinstance(sys.exc_info()[1], (ConnectionResetError, BrokenPipeError)):
            super().handle_error(request, client_address)


class Origin(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        self.send_response(200)
        self.send_header('Content-Length', str(SIZE))
        self.end_headers()
        self.wfile.write(DOWNLOAD)

    def do_POST(self):
        digest, remaining = hashlib.sha256(), int(self.headers['Content-Length'])
        while remaining:
            chunk = self.rfile.read(min(1 << 16, remaining))
            if not chunk:
                break
            digest.update(chunk)
            remaining -= len(chunk)
        body = json.dumps({'sha256': digest.hexdigest(), 'missing': remaining}).encode()
        self.send_response(200)
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def main():
    before = registration()
    name = 'smoke-tunnel-xfer-' + uuid.uuid4().hex[:10]
    identifier = None
    server = QuietServer(('127.0.0.1', 0), Origin)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        created = run('tunnel', 'create', name, '--port', str(server.server_port), '--duration', '30m',
                      '--wait', '--timeout', '90s')
        identifier = created['tunnel']['id']
        endpoint = created['tunnel']['stable_endpoint']
        wait(lambda: urllib.request.urlopen(endpoint + '/', timeout=60).read(1), 'tunnel to serve')
        with urllib.request.urlopen(endpoint + '/', timeout=120) as response:
            downloaded = response.read()
        assert hashlib.sha256(downloaded).digest() == hashlib.sha256(DOWNLOAD).digest(), 'download differs'
        upload = os.urandom(SIZE)
        request = urllib.request.Request(endpoint + '/upload', data=upload, method='POST')
        with urllib.request.urlopen(request, timeout=180) as response:
            result = json.loads(response.read())
        assert result == {'sha256': hashlib.sha256(upload).hexdigest(), 'missing': 0}, 'upload differs: %s' % result
    finally:
        try:
            if identifier is not None:
                confirmed('tunnel', 'delete', identifier)
        finally:
            server.shutdown()
            server.server_close()
    assert registration() == before
    print(json.dumps({'check': 'tunnel-transfer', 'status': 'PASS', 'upload_16mib': True,
                      'download_16mib': True, 'enrollment_preserved': True}))


if __name__ == '__main__':
    main()
