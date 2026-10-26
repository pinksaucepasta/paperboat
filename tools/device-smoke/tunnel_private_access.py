#!/usr/bin/env python3
"""C126: access one private TCP route by route/tunnel ID, exchange bytes, close the listener."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import tempfile
import time

from pairing_lifecycle import command_result


def require(ok, message):
    if not ok:
        raise RuntimeError(message)


def error_case(pb, words):
    result = command_result(pb, *words, input='', timeout=45)
    text = result.stdout + result.stderr
    passed = result.returncode != 0 and bool(text.strip()) and not any(x in text for x in
                 ('panic:', 'goroutine ', 'Traceback', 'nil pointer'))
    record = {'argv': [str(pb), *words], 'exit': result.returncode,
              'status': 'PASS' if passed else 'FAIL'}
    if not passed:
        record.update(stdout=result.stdout, stderr=result.stderr)
    return record


def one_session(pb, selector, tunnel, route, nonce):
    argv = [str(pb), 'access', 'tunnel', selector, '--listen', '127.0.0.1:0', '--json']
    with tempfile.TemporaryFile() as output, tempfile.TemporaryFile() as errors:
        process = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=output, stderr=errors)
        result = {'argv': argv}
        session = None
        try:
            end = time.monotonic() + 60
            while time.monotonic() < end:
                raw = os.pread(output.fileno(), (16 << 10) + 1, 0)
                require(len(raw) <= 16 << 10, 'access receipt exceeded bound')
                if b'\n' in raw:
                    receipt = json.loads(raw.split(b'\n', 1)[0])
                    require(receipt.get('ok') is not False, 'access was refused')
                    session = receipt.get('data', receipt)
                    break
                require(process.poll() is None, 'access command exited before its receipt')
                time.sleep(.1)
            require(session is not None, 'access command did not publish its listener')
            require(session['tunnel_id'] == tunnel and session['route_id'] == route, 'wrong access resource')
            host, port = session['listen_address'].rsplit(':', 1)
            require(host == '127.0.0.1' and int(port) > 0, 'listener is not literal loopback')
            require(process.poll() is None, 'access command did not retain its session')
            payload = nonce.encode() + b'\x00' + bytes(range(256)) * 16
            with socket.create_connection((host, int(port)), timeout=20) as connection:
                connection.settimeout(20)
                connection.sendall(payload)
                answer = bytearray()
                while len(answer) < len(payload):
                    chunk = connection.recv(min(4096, len(payload) - len(answer)))
                    require(chunk, 'TCP echo closed early')
                    answer.extend(chunk)
                require(bytes(answer) == payload, 'TCP echo changed bytes')
            require(process.poll() is None, 'access exited during echo')
            process.send_signal(signal.SIGTERM)
            process.wait(timeout=15)
            with socket.socket() as probe:
                probe.settimeout(2)
                require(probe.connect_ex((host, int(port))) != 0, 'listener remained open after command exit')
            result.update(status='PASS', payload_bytes=len(payload),
                          payload_sha256=hashlib.sha256(payload).hexdigest(),
                          listener_closed=True, shutdown_exit=process.returncode)
        except Exception as error:
            result.update(status='FAIL', reason=str(error),
                          stdout=os.pread(output.fileno(), 64 << 10, 0).decode(errors='replace'),
                          stderr=os.pread(errors.fileno(), 64 << 10, 0).decode(errors='replace'))
        finally:
            if process.poll() is None:
                process.send_signal(signal.SIGTERM)
                try:
                    process.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
        return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--tunnel', required=True)
    parser.add_argument('--route', required=True)
    parser.add_argument('--nonce', required=True)
    args = parser.parse_args()
    pb = Path.home() / '.local/bin/pb'
    errors = [error_case(pb, ['access', 'tunnel', '--json']),
              error_case(pb, ['access', 'tunnel', args.route, '--listen', '0.0.0.0:0', '--json']),
              error_case(pb, ['access', 'tunnel', 'missing-' + args.nonce, '--json'])]
    helped = command_result(pb, 'access', 'tunnel', '--help', timeout=30)
    help_ok = helped.returncode == 0 and 'Usage:' in helped.stdout and 'loopback' in helped.stdout
    sessions = [one_session(pb, selector, args.tunnel, args.route, args.nonce)
                for selector in [args.route, args.tunnel]]
    passed = help_ok and all(x['status'] == 'PASS' for x in [*errors, *sessions])
    print(json.dumps({'check': 'tunnel-private-access', 'row': 'C126',
                      'status': 'PASS' if passed else 'FAIL', 'help': help_ok,
                      'errors': errors, 'sessions': sessions}))
    raise SystemExit(0 if passed else 1)


if __name__ == '__main__':
    main()
