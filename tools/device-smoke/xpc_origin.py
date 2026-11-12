"""Origin fixture that runs ON a device of the X rows (python3 on L and M, python on W), standard library only.

  xpc_origin.py http|https BIND PORT MARKER [CERT KEY]   HTTP/1.1 (TLS with CERT and KEY for https)
  xpc_origin.py tcp BIND PORT                            echo server: answers b"echo:" + what it read; "STOP" ends it
  xpc_origin.py tls BIND PORT CERT KEY                   TLS server that answers b"tls-origin-ok"

http paths: /marker (also /), /big (16 MiB), /sse (5 events 0.3 s apart), /ws (WebSocket echo), POST /digest (16 MiB: the sha256
of what arrived), /xff (JSON: Host and X-Forwarded-For as the origin saw them), /__stop. It ends itself after 5 minutes.
"""
import base64
import hashlib
import http.server
import json
import socket
import socketserver
import ssl
import struct
import sys
import threading
import time

mode, bind, port = sys.argv[1], sys.argv[2], int(sys.argv[3])
FAMILY = socket.AF_INET6 if ':' in bind else socket.AF_INET
BIG = (b'0123456789abcdef' * 64 + b'\n') * 16384


def wrap(server, cert, key):
    """TLS is wrapped per connection in the handler thread: a client that connects and never handshakes cannot block accept()."""
    server.tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    server.tls.load_cert_chain(cert, key)


class Http(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, *args):
        pass

    def reply(self, body, ctype='text/plain'):
        self.send_response(200)
        self.send_header('Content-Type', ctype)
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == '/__stop':
            self.reply(b'stopping')
            threading.Thread(target=self.server.shutdown, daemon=True).start()
        elif self.path == '/big':
            self.reply(BIG, 'application/octet-stream')
        elif self.path == '/xff':
            self.reply(json.dumps({'host': self.headers.get('Host'), 'xff': self.headers.get('X-Forwarded-For')}).encode(), 'application/json')
        elif self.path == '/sse':
            self.send_response(200)
            self.send_header('Content-Type', 'text/event-stream')
            self.send_header('Transfer-Encoding', 'chunked')
            self.end_headers()
            for i in range(5):
                event = ('data: %d\n\n' % i).encode()
                self.wfile.write(b'%x\r\n%s\r\n' % (len(event), event))
                self.wfile.flush()
                time.sleep(0.3)
            self.wfile.write(b'0\r\n\r\n')
        elif self.path == '/ws' and self.headers.get('Upgrade', '').lower() == 'websocket':
            key = self.headers.get('Sec-WebSocket-Key', '')
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
            self.reply(self.server.marker.encode())

    def do_POST(self):
        length, digest = int(self.headers.get('Content-Length', '0')), hashlib.sha256()
        while length:
            chunk = self.rfile.read(min(length, 65536))
            if not chunk:
                return
            digest.update(chunk)
            length -= len(chunk)
        self.reply(digest.hexdigest().encode())

    def exact(self, count):
        data = self.rfile.read(count)
        if len(data) != count:
            raise EOFError
        return data

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
                length = second & 127
                if length == 126:
                    length = struct.unpack('!H', self.exact(2))[0]
                elif length == 127:
                    length = struct.unpack('!Q', self.exact(8))[0]
                mask = self.exact(4)
                body = bytes(v ^ mask[i % 4] for i, v in enumerate(self.exact(length)))
            except (EOFError, OSError):
                return
            opcode = first & 15
            if opcode == 8:
                self.frame(8, body)
                return
            if opcode == 9:
                self.frame(10, body)
            elif opcode in (0, 2):
                message.extend(body)
                if first & 128:
                    self.frame(2, bytes(message))
                    message.clear()


class Echo(socketserver.BaseRequestHandler):
    def handle(self):
        data = self.request.recv(100)
        if data.strip() == b'STOP':
            threading.Thread(target=self.server.shutdown, daemon=True).start()
            return
        self.request.sendall(b'echo:' + data)


class Hello(socketserver.BaseRequestHandler):
    def handle(self):
        self.request.recv(100)
        self.request.sendall(b'tls-origin-ok')


class V6Mixin:
    address_family = FAMILY
    daemon_threads = True
    allow_reuse_address = True


class TlsMixin:
    tls = None

    def finish_request(self, request, client_address):
        if self.tls:
            request.settimeout(10)
            request = self.tls.wrap_socket(request, server_side=True)
        super().finish_request(request, client_address)


class HttpServer(TlsMixin, V6Mixin, socketserver.ThreadingMixIn, http.server.HTTPServer):
    pass


class TcpServer(TlsMixin, V6Mixin, socketserver.ThreadingMixIn, socketserver.TCPServer):
    pass


if mode in ('http', 'https'):
    server = HttpServer((bind, port), Http)
    server.marker = sys.argv[4]
    if mode == 'https':
        wrap(server, sys.argv[5], sys.argv[6])
elif mode == 'tcp':
    server = TcpServer((bind, port), Echo)
else:
    server = TcpServer((bind, port), Hello)
    wrap(server, sys.argv[4], sys.argv[5])
threading.Timer(300, server.shutdown).start()
server.serve_forever()
