#!/usr/bin/env python3
"""Qualify private TCP through the CLI forward and the assigned loopback address."""
import argparse
import errno
import hashlib
import http.server
import ipaddress
import json
import os
from pathlib import Path
import select
import signal
import socket
import socketserver
import subprocess
import tempfile
import threading
import time
import uuid

from install_lifecycle import registration
from pairing_lifecycle import command_diagnostic, invoke

PAYLOAD = ('private TCP λ船\n'.encode() + bytes(range(256))) * 1024


class Echo(socketserver.BaseRequestHandler):
    def handle(self):
        self.request.settimeout(20)
        body = bytearray()
        while len(body) <= len(PAYLOAD):
            chunk = self.request.recv(65536)
            if not chunk:
                break
            body.extend(chunk)
        if not body:  # The machine's service scanner probes newly listening ports.
            return
        if bytes(body) != PAYLOAD:
            raise RuntimeError('private origin received different synthetic bytes')
        self.request.sendall(self.server.marker.encode() + hashlib.sha256(PAYLOAD).digest())


class HTTPOrigin(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = self.server.marker.encode() + hashlib.sha256(PAYLOAD).digest()
        self.send_response(200)
        self.send_header('Content-Type', 'application/octet-stream')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_):
        pass


def origin(args):
    before = registration()
    server = (http.server.ThreadingHTTPServer(('127.0.0.1', 0), HTTPOrigin)
              if args.check == 'private-https' else
              socketserver.ThreadingTCPServer(('127.0.0.1', 0), Echo))
    server.daemon_threads = True
    server.marker = 'PB_PRIVATE_' + uuid.uuid4().hex
    worker = threading.Thread(target=server.serve_forever, daemon=True)
    worker.start()
    try:
        with tempfile.TemporaryDirectory(prefix='paperboat-private-') as directory:
            gate, failed = Path(directory) / 'verified', Path(directory) / 'failed'
            print(json.dumps({'state': 'private_access_required', 'check': args.check,
                              'target': args.target, 'machine_id': before['machine_id'],
                              'port': server.server_address[1], 'marker': server.marker,
                              'gate': str(gate), 'failure_gate': str(failed)}), flush=True)
            deadline = time.monotonic() + 150
            while not gate.exists():
                if failed.exists():
                    raise RuntimeError('private TCP client check failed; private receipt retained')
                if time.monotonic() >= deadline:
                    raise RuntimeError('private TCP controller exceeded its handoff bound')
                time.sleep(.1)
    finally:
        server.shutdown()
        server.server_close()
        worker.join(timeout=5)
    assert registration() == before
    print(json.dumps({'check': 'private-origin', 'status': 'PASS',
                      'enrollment_unchanged': True, 'origin_closed': True}), flush=True)


class ExchangeMismatch(AssertionError):
    pass


def exchange(address, phase):
    with socket.create_connection(address, timeout=20) as connection:
        connection.sendall(PAYLOAD)
        connection.shutdown(socket.SHUT_WR)
        received = bytearray()
        expected = phase['marker'].encode() + hashlib.sha256(PAYLOAD).digest()
        while len(received) <= len(expected):
            chunk = connection.recv(4096)
            if not chunk:
                break
            received.extend(chunk)
        if bytes(received) != expected:
            # The marker and digest are synthetic, so the received prefix is safe to record.
            raise ExchangeMismatch('private TCP changed bytes or lost half-close delivery: received %d of %d bytes, first 64 %r'
                                   % (len(received), len(expected), bytes(received[:64])))


def connectivity(binary):
    """What this computer could reach when a check failed, for the private diagnostics."""
    try:
        result = subprocess.run([str(binary), 'doctor', '--json'], capture_output=True, timeout=45)
        return 'doctor exit %d\n%s%s' % (result.returncode, result.stdout.decode(errors='replace'),
                                        result.stderr.decode(errors='replace'))
    except (OSError, subprocess.SubprocessError) as error:
        return 'doctor could not run: %r' % (error,)


def failure_diagnostic(binary, arguments, output, errors, error):
    detail = '%s: %s\n--- connectivity at failure ---\n%s' % (type(error).__name__, error, connectivity(binary))
    return command_diagnostic(arguments, output, errors + b'\n' + detail.encode())


def forward(binary, phase):
    arguments = [str(binary), 'access', 'machine', phase['machine_id'], '--port',
                 str(phase['port']), '--listen', '127.0.0.1:0', '--json']
    with tempfile.TemporaryFile() as errors:
        child = subprocess.Popen(arguments, stdout=subprocess.PIPE, stderr=errors)
        output = bytearray()
        try:
            deadline = time.monotonic() + 30
            while b'\n' not in output:
                if time.monotonic() >= deadline:
                    raise RuntimeError('machine access did not publish its local listener')
                if select.select([child.stdout], [], [], .1)[0]:
                    chunk = os.read(child.stdout.fileno(), 4096)
                    if not chunk:
                        raise RuntimeError('machine access ended before listener admission')
                    output.extend(chunk)
                    assert len(output) <= 65536, 'machine access exceeded its receipt bound'
            receipt = json.loads(output.split(b'\n', 1)[0])
            receipt = receipt.get('data', receipt)
            assert receipt['schema'] == 'paperboat.machine-access/v1'
            assert receipt['machine_id'] == phase['machine_id'] and receipt['remote_port'] == phase['port']
            host, port = receipt['listen_address'].rsplit(':', 1)
            assert host == '127.0.0.1' and 0 < int(port) <= 65535
            exchange((host, int(port)), phase)
            assert child.poll() is None, 'machine access ended while its foreground owner was live'
        except BaseException as error:
            errors.seek(0)
            path = failure_diagnostic(binary, arguments, output, errors.read(1 << 20), error)
            raise RuntimeError('private machine access failed; private diagnostics: ' + str(path)) from error
        finally:
            if child.poll() is None:
                child.send_signal(signal.SIGTERM)
                try:
                    child.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait(timeout=5)
            child.stdout.close()


def probe(args):
    phase = json.loads(args.phase)
    assert phase['state'] == 'private_access_required' and phase['check'] == args.check
    before = registration()
    binary = Path.home() / '.local/bin/pb'
    if args.check == 'access-machine':
        forward(binary, phase)
    elif args.check == 'private-https':
        machines = invoke(binary, 'machine', 'list', '--json')['machines']
        matches = [m for m in machines if m['id'] == phase['machine_id']]
        assert len(matches) == 1 and matches[0]['network_approved']
        alias = matches[0]['alias']
        domain = invoke(binary, 'config', 'local-access', 'show', '--json')['local_access']['domain']
        assert alias and domain and not any(c in alias + domain for c in '/: \r\n')
        expected = phase['marker'].encode() + hashlib.sha256(PAYLOAD).digest()
        hosts = [str(phase['port']) + '.' + alias + '.' + domain,
                 alias + '.' + domain + ':' + str(phase['port'])]
        for host in hosts:
            command = ['curl', '--silent', '--show-error', '--fail', '--noproxy', '*',
                       '--connect-timeout', '15', '--max-time', '30', 'https://' + host + '/']
            result = subprocess.run(command, capture_output=True, timeout=35)
            if result.returncode or result.stdout != expected:
                path = command_diagnostic(command, b'', result.stderr)
                raise RuntimeError('trusted private HTTPS failed; private diagnostics: ' + str(path))
    else:
        machines = invoke(binary, 'machine', 'list', '--json')['machines']
        matches = [m for m in machines if m['id'] == phase['machine_id']]
        assert len(matches) == 1 and matches[0]['network_approved'], 'private target is not approved in inventory'
        address = ipaddress.ip_address(matches[0]['assigned_ip'])
        assert address in ipaddress.ip_network('127.100.0.0/16'), 'machine has no assigned private loopback address'
        deadline = time.monotonic() + 45
        while True:
            try:
                exchange((str(address), phase['port']), phase)
                break
            except OSError as error:
                if error.errno not in (errno.ECONNREFUSED, errno.EADDRNOTAVAIL) or time.monotonic() >= deadline:
                    raise
                time.sleep(.25)
            except ExchangeMismatch as error:
                path = failure_diagnostic(binary, ['exchange', str(address), str(phase['port'])], b'', b'', error)
                raise RuntimeError('private address exchange failed; private diagnostics: ' + str(path)) from error
    assert registration() == before
    print(json.dumps({'check': args.check, 'target': phase['target'], 'status': 'PASS',
                      'exact_bytes': True, 'half_close': args.check != 'private-https',
                      'trusted_https': args.check == 'private-https', 'enrollment_unchanged': True}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['origin', 'probe'])
    parser.add_argument('--target')
    parser.add_argument('--phase')
    parser.add_argument('--check', choices=['access-machine', 'private-address', 'private-https'], required=True)
    args = parser.parse_args()
    (origin if args.action == 'origin' else probe)(args)


if __name__ == '__main__':
    main()
