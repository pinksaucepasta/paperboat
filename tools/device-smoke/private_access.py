#!/usr/bin/env python3
"""Qualify private TCP through the CLI forward and the assigned loopback address."""
import argparse
import errno
import hashlib
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


def origin(args):
    before = registration()
    server = socketserver.ThreadingTCPServer(('127.0.0.1', 0), Echo)
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
        assert bytes(received) == expected, 'private TCP changed bytes or lost half-close delivery'


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
            path = command_diagnostic(arguments, output, errors.read(1 << 20))
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
    assert registration() == before
    print(json.dumps({'check': args.check, 'target': phase['target'], 'status': 'PASS',
                      'exact_bytes': True, 'half_close': True, 'enrollment_unchanged': True}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['origin', 'probe'])
    parser.add_argument('--target')
    parser.add_argument('--phase')
    parser.add_argument('--check', choices=['access-machine', 'private-address'], required=True)
    args = parser.parse_args()
    (origin if args.action == 'origin' else probe)(args)


if __name__ == '__main__':
    main()
