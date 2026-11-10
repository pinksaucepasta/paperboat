#!/usr/bin/env python3
"""C126: access one private TCP route by route/tunnel ID, exchange bytes, close the listener."""
import argparse
import base64
import hashlib
import json
import os
import re
import select
import socket
import time

import winhost
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


def echo(host, port, payload):
    """Send payload to the private listener and return what came back, from the device when pb runs there."""
    if winhost.device():
        rc, out = winhost.device().ps('tunnel echo with %s:%s' % (host, port), winhost.ECHO, timeout=90,
                                      address=host, port=port, block=base64.b64encode(payload).decode())
        return base64.b64decode(out)
    with socket.create_connection((host, port), timeout=20) as connection:
        connection.settimeout(20)
        connection.sendall(payload)
        answer = bytearray()
        while len(answer) < len(payload):
            chunk = connection.recv(min(4096, len(payload) - len(answer)))
            require(chunk, 'TCP echo closed early')
            answer.extend(chunk)
        return bytes(answer)


def listener_closed(host, port):
    if winhost.device():
        rc, _ = winhost.device().ps('tunnel listener closed', winhost.EXCHANGE, timeout=60, ok=(0, winhost.REFUSED),
                                    address=host, port=port, block='AA==', repeat=1)
        return rc == winhost.REFUSED
    with socket.socket() as probe:
        probe.settimeout(2)
        return probe.connect_ex((host, port)) != 0


class ConsoleJob:
    """pb.exe on a console (ssh -tt gives it a ConPTY): the job whose Ctrl-C reaches the command, so its listener
    is released as on Unix. taskkill would skip that release and the daemon would keep the listener."""

    def __init__(self, pb, argv):
        from terminal_lifecycle import Terminal  # a Windows device only; the Unix fixture tarball has no pty harness
        self.terminal = Terminal(winhost.terminal_argv(pb, argv), env=winhost.terminal_env(pb))

    def stdout(self, limit=(1 << 20) + 1):
        while select.select([self.terminal.fd], [], [], 0)[0]:
            try:
                self.terminal.drain()
            except RuntimeError:
                break
        text = re.sub(rb'\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07]*\x07|\r', b'', bytes(self.terminal.output))
        return text.lstrip()[:limit]

    def stderr(self):
        return b''

    def poll(self):
        if self.terminal.status is None:
            done, status = os.waitpid(self.terminal.pid, os.WNOHANG)
            if done:
                self.terminal.status = os.waitstatus_to_exitcode(status)
        return self.terminal.status

    def terminate(self):
        self.terminal.send(b'\x03')
        self.terminal.wait_exit(20)

    def kill(self):
        self.terminal.close()

    def close(self):
        self.terminal.close()


def one_session(pb, selector, tunnel, route, nonce):
    argv = ['access', 'tunnel', selector, '--listen', '127.0.0.1:0', '--json']
    job = ConsoleJob(pb, argv) if winhost.device() else winhost.job(pb, argv)
    result = {'argv': [str(pb), *argv]}
    try:
        session = None
        end = time.monotonic() + 60
        while time.monotonic() < end:
            raw = job.stdout((16 << 10) + 1)
            require(len(raw) <= 16 << 10, 'access receipt exceeded bound')
            if b'\n' in raw:
                receipt = json.loads(raw.split(b'\n', 1)[0])
                require(receipt.get('ok') is not False, 'access was refused')
                session = receipt.get('data', receipt)
                break
            require(job.poll() is None, 'access command exited before its receipt')
            time.sleep(.5 if winhost.device() else .1)
        require(session is not None, 'access command did not publish its listener')
        require(session['tunnel_id'] == tunnel and session['route_id'] == route, 'wrong access resource')
        host, port = session['listen_address'].rsplit(':', 1)
        port = int(port)
        require(host == '127.0.0.1' and port > 0, 'listener is not literal loopback')
        require(job.poll() is None, 'access command did not retain its session')
        payload = nonce.encode() + b'\x00' + bytes(range(256)) * 16
        require(echo(host, port, payload) == payload, 'TCP echo changed bytes')
        require(job.poll() is None, 'access exited during echo')
        job.terminate()
        end = time.monotonic() + 15  # a killed Windows process closes its sockets a moment after taskkill returns
        while not listener_closed(host, port):
            require(time.monotonic() < end, 'listener remained open after command exit')
            time.sleep(1)
        result.update(status='PASS', payload_bytes=len(payload), payload_sha256=hashlib.sha256(payload).hexdigest(),
                      listener_closed=True, shutdown_exit=job.poll())
    except Exception as error:
        result.update(status='FAIL', reason=str(error), stdout=job.stdout(64 << 10).decode(errors='replace'),
                      stderr=job.stderr().decode(errors='replace'))
    finally:
        if job.poll() is None:
            job.kill()
        job.close()
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--tunnel', required=True)
    parser.add_argument('--route', required=True)
    parser.add_argument('--nonce', required=True)
    args = parser.parse_args()
    pb = winhost.pb_path()
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
