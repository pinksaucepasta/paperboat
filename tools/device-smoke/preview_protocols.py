#!/usr/bin/env python3
"""Publish a bounded synthetic HTTP/WebSocket origin for an external protocol probe."""
import argparse
import base64
import hashlib
import http.server
import json
from pathlib import Path
import struct
import tempfile
import threading
import time
import uuid

from install_lifecycle import registration
from pairing_lifecycle import invoke
from preview_receipt import PREVIEW_COMMAND_TIMEOUT, receipt_value


class ProtocolOrigin(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, *args):
        pass

    def reply(self, status, body=b''):
        self.send_response(status)
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == '/marker':
            self.reply(200, self.server.marker.encode())
        elif self.path == '/ws' and self.headers.get('Upgrade', '').lower() == 'websocket':
            key = self.headers.get('Sec-WebSocket-Key', '')
            if len(base64.b64decode(key, validate=True)) != 16:
                self.reply(400)
                return
            accepted = base64.b64encode(hashlib.sha1((key + '258EAFA5-E914-47DA-95CA-C5AB0DC85B11').encode()).digest()).decode()
            self.send_response(101)
            self.send_header('Upgrade', 'websocket')
            self.send_header('Connection', 'Upgrade')
            self.send_header('Sec-WebSocket-Accept', accepted)
            self.end_headers()
            self.wfile.flush()
            self.connection.settimeout(15)
            self.echo_socket()
        else:
            self.reply(404)

    def do_POST(self):
        length = int(self.headers.get('Content-Length', '0'))
        if self.path != '/digest' or length != 16 << 20:
            self.reply(400)
            return
        digest = hashlib.sha256()
        while length:
            chunk = self.rfile.read(min(length, 64 << 10))
            if not chunk:
                raise RuntimeError('preview truncated the synthetic request body')
            digest.update(chunk)
            length -= len(chunk)
        self.reply(200, digest.hexdigest().encode())

    def exact(self, length):
        body = self.rfile.read(length)
        if len(body) != length:
            raise EOFError('WebSocket closed during its frame')
        return body

    def frame(self, opcode, body):
        prefix = bytes([0x80 | opcode])
        if len(body) < 126:
            prefix += bytes([len(body)])
        elif len(body) <= 65535:
            prefix += bytes([126]) + struct.pack('!H', len(body))
        else:
            prefix += bytes([127]) + struct.pack('!Q', len(body))
        self.wfile.write(prefix + body)
        self.wfile.flush()

    def echo_socket(self):
        message = bytearray()
        while True:
            try:
                first, second = self.exact(2)
            except EOFError:
                return
            length = second & 127
            if length == 126:
                length = struct.unpack('!H', self.exact(2))[0]
            elif length == 127:
                length = struct.unpack('!Q', self.exact(8))[0]
            if not second & 128 or length > 1 << 20:
                raise RuntimeError('unmasked or oversized fixture WebSocket frame')
            mask = self.exact(4)
            body = bytes(value ^ mask[index % 4] for index, value in enumerate(self.exact(length)))
            opcode = first & 15
            if opcode == 8:
                self.frame(8, body)
                return
            if opcode == 9:
                self.frame(10, body)
                continue
            if opcode == 10:
                continue
            if opcode not in (0, 2) or len(message) + len(body) > 1 << 20:
                raise RuntimeError('unexpected fixture WebSocket message')
            message.extend(body)
            if first & 128:
                self.frame(2, message)
                message.clear()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    before = registration()
    binary = Path.home() / '.local/bin/pb'
    server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), ProtocolOrigin)
    server.marker = 'PB_PREVIEW_PROTOCOL_' + uuid.uuid4().hex
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    lease = None
    failure = None
    try:
        with tempfile.TemporaryDirectory(prefix='paperboat-preview-protocol-') as directory:
            root = Path(directory)
            lease = invoke(binary, 'preview', str(server.server_port), '--background', '--ttl', '5m', '--json', timeout=PREVIEW_COMMAND_TIMEOUT)
            endpoint = receipt_value(lease, 'endpoint')
            gate, refused = root / 'verified', root / 'failed'
            print(json.dumps({'state': 'preview_protocol_required', 'target': args.target,
                              'url': endpoint, 'marker': server.marker,
                              'gate': str(gate), 'failure_gate': str(refused)}), flush=True)
            deadline = time.monotonic() + 180
            while not gate.exists():
                if refused.exists():
                    raise RuntimeError('external preview protocol probe failed')
                if time.monotonic() >= deadline:
                    raise RuntimeError('external protocol probe exceeded180s')
                time.sleep(.1)
    except Exception as error:
        failure = error
        raise
    finally:
        try:
            if lease is not None:
                invoke(binary, 'preview', 'stop', lease['id'], '--json', timeout=30)
        except Exception as error:
            if failure is None:
                raise
            failure.add_note('Exact protocol preview cleanup failed: ' + str(error))
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)
    if registration() != before:
        raise RuntimeError('preview protocol probe changed the enrolled identity')
    print(json.dumps({'check': 'preview-protocols', 'target': args.target, 'status': 'PASS',
                      'enrollment_unchanged': True, 'exact_cleanup': True}), flush=True)


if __name__ == '__main__':
    main()
