"""One-endpoint CONNECT relay for bounded real-device network-loss injection.

TLS stays end to end. This helper never decodes or logs tunneled traffic.
"""
import select
import socket
import threading
import time


class PairingFaultProxy:
    def __init__(self, host, port):
        self.target = (host, port)
        self.authority = host + ':' + str(port)
        self.stopped = threading.Event()
        self.lock = threading.Lock()
        self.live = set()
        self.workers = []
        self.listener = socket.socket()
        self.listener.bind(('127.0.0.1', 0))
        self.listener.listen(16)
        self.listener.settimeout(0.2)
        self.address = self.listener.getsockname()
        self.thread = threading.Thread(target=self.accept, daemon=True)
        self.thread.start()

    def accept(self):
        while not self.stopped.is_set():
            try:
                client, _ = self.listener.accept()
            except socket.timeout:
                continue
            except OSError:
                break
            with self.lock:
                self.workers = [worker for worker in self.workers if worker.is_alive()]
                if len(self.workers) >= 16:
                    client.close()
                    continue
                worker = threading.Thread(target=self.forward, args=(client,), daemon=True)
                self.workers.append(worker)
            worker.start()

    def forward(self, client):
        upstream = None
        pair = None
        try:
            client.settimeout(5)
            header = bytearray()
            while not header.endswith(b'\r\n\r\n') and len(header) < 8192:
                value = client.recv(1)
                if not value:
                    return
                header.extend(value)
            first = bytes(header).split(b'\r\n', 1)[0].decode('ascii')
            if not header.endswith(b'\r\n\r\n') or first != 'CONNECT ' + self.authority + ' HTTP/1.1':
                client.sendall(b'HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n')
                return
            upstream = socket.create_connection(self.target, timeout=5)
            pair = (client, upstream)
            with self.lock:
                self.live.add(pair)
            client.sendall(b'HTTP/1.1 200 Connection Established\r\n\r\n')
            while not self.stopped.is_set():
                readable, _, _ = select.select(pair, [], [], 0.2)
                for source in readable:
                    data = source.recv(64 << 10)
                    if not data:
                        return
                    (upstream if source is client else client).sendall(data)
        except (OSError, ValueError, UnicodeError):
            pass
        finally:
            with self.lock:
                self.live.discard(pair)
            client.close()
            if upstream is not None:
                upstream.close()

    def cut(self):
        with self.lock:
            pairs = list(self.live)
        for pair in pairs:
            for connection in pair:
                try:
                    connection.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass
        return len(pairs)

    def live_count(self):
        with self.lock:
            return len(self.live)

    def close(self):
        self.stopped.set()
        self.listener.close()
        self.cut()
        self.thread.join(timeout=1)
        deadline = time.monotonic() + 6
        for worker in self.workers:
            worker.join(timeout=max(0, deadline - time.monotonic()))
