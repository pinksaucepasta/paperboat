#!/usr/bin/env python3
"""Qualify private local access (F9) from one paired controller machine to one target machine.

Runs from any host that can ssh to the controller. The controller drives every check with
`pb`; the target only runs a synthetic origin through `pb exec`, so it is reached the way a
user reaches it. Prints one PASS/FAIL line per check id and exits non-zero on any failure.

  python3 local_access.py --controller pb@pbvm2 --target hp [--only ID,ID]
"""
import argparse
import base64
import json
import shlex
import subprocess
import sys
import time
import uuid

ORIGIN = r'''
import hashlib, http.server, json, socketserver, sys, threading, time
PAYLOAD = ('private TCP λ船\n'.encode() + bytes(range(256))) * 1024
reply = sys.argv[1].encode() + hashlib.sha256(PAYLOAD).digest()
class Echo(socketserver.BaseRequestHandler):
    def handle(self):
        self.request.settimeout(20)
        body = bytearray()
        while len(body) <= len(PAYLOAD):
            chunk = self.request.recv(65536)
            if not chunk:
                break
            body.extend(chunk)
        if bytes(body) == PAYLOAD:
            self.request.sendall(reply)
class Web(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.send_header('Content-Length', str(len(reply))); self.end_headers()
        self.wfile.write(reply)
    def log_message(self, *_):
        pass
socketserver.ThreadingTCPServer.daemon_threads = True
tcp = socketserver.ThreadingTCPServer(('127.0.0.1', 0), Echo)
web = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Web)
for server in (tcp, web):
    threading.Thread(target=server.serve_forever, daemon=True).start()
print(json.dumps({'tcp': tcp.server_address[1], 'http': web.server_address[1]}), flush=True)
time.sleep(float(sys.argv[2]))
'''

PROBE = r'''
import hashlib, json, os, select, signal, socket, subprocess, sys, time
PAYLOAD = ('private TCP λ船\n'.encode() + bytes(range(256))) * 1024
check, ip, port, alias, machine, marker = sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4], sys.argv[5], sys.argv[6]
expected = marker.encode() + hashlib.sha256(PAYLOAD).digest()

def eventually(action, seconds=60):
    end = time.time() + seconds
    while True:
        try:
            return action()
        except (OSError, AssertionError):
            if time.time() > end:
                raise
            time.sleep(.5)

def exchange(address):
    with socket.create_connection(address, timeout=20) as connection:
        connection.sendall(PAYLOAD)
        connection.shutdown(socket.SHUT_WR)
        received = bytearray()
        while len(received) <= len(expected):
            chunk = connection.recv(4096)
            if not chunk:
                break
            received.extend(chunk)
    assert bytes(received) == expected, 'private TCP changed bytes or lost half-close delivery'

def explicit():
    child = subprocess.Popen(['pb', 'access', 'machine', machine, '--port', str(port), '--listen', '127.0.0.1:0', '--json'],
                             stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    try:
        output, end = b'', time.time() + 40
        while b'\n' not in output:
            assert time.time() < end and child.poll() is None, 'machine access did not publish its listener'
            if select.select([child.stdout], [], [], .2)[0]:
                output += os.read(child.stdout.fileno(), 4096)
        receipt = json.loads(output.split(b'\n', 1)[0])
        receipt = receipt.get('data', receipt)
        assert receipt['schema'] == 'paperboat.machine-access/v1' and receipt['remote_port'] == port
        host, local = receipt['listen_address'].rsplit(':', 1)
        assert host == '127.0.0.1'
        exchange((host, int(local)))
        assert child.poll() is None, 'machine access ended while its foreground owner was live'
    finally:
        if child.poll() is None:
            child.send_signal(signal.SIGTERM)
        child.wait(timeout=15)

def https(form):
    domain = json.loads(subprocess.check_output(['pb', 'config', 'local-access', 'show', '--json']))['data']['local_access']['domain']
    host = f'{port}.{alias}.{domain}' if form == 'port-host' else f'{alias}.{domain}:{port}'
    result = subprocess.run(['curl', '-sS', '--fail', '--noproxy', '*', '--connect-timeout', '15', '--max-time', '30',
                             'https://' + host + '/'], capture_output=True, timeout=40)
    assert result.returncode == 0 and result.stdout == expected, (result.returncode, result.stderr[:160])

actions = {
    'private-address': lambda: exchange((ip, port)),
    'explicit-listen': explicit,
    'https-port-host': lambda: https('port-host'),
    'https-host-port': lambda: https('host-port'),
}
eventually(actions[check])
print('PROBE_OK')
'''


def ssh(controller, command, timeout=120, **kwargs):
    return subprocess.run(['ssh', '-o', 'BatchMode=yes', controller, 'export PATH=$HOME/.local/bin:$PATH; ' + command],
                          capture_output=True, text=True, timeout=timeout, **kwargs)


def pb_json(controller, *arguments):
    result = ssh(controller, ' '.join(shlex.quote(a) for a in ('pb', *arguments, '--json')))
    assert result.returncode == 0, result.stderr[-300:]
    receipt = json.loads(result.stdout)
    return receipt.get('data', receipt)


class Origin:
    def __init__(self, controller, target, seconds=900):
        self.controller, self.target, self.marker = controller, target, 'PB_LA_' + uuid.uuid4().hex
        self.log = f'/tmp/local-access-{uuid.uuid4().hex[:8]}'
        script = base64.b64encode(ORIGIN.encode()).decode()
        remote = f'import base64;exec(base64.b64decode("{script}").decode())'
        command = (f'setsid nohup pb exec {shlex.quote(target)} --timeout {seconds + 60}s -- python3 -c {shlex.quote(remote)} '
                   f'{self.marker} {seconds} > {self.log}.out 2>&1 < /dev/null & echo $! > {self.log}.pid')
        assert ssh(controller, command).returncode == 0
        for _ in range(60):
            out = ssh(controller, f'cat {self.log}.out').stdout.strip()
            if out.startswith('{'):
                self.ports = json.loads(out.splitlines()[0])
                return
            time.sleep(1)
        raise RuntimeError('target origin did not start: ' + out[-200:])

    def close(self):
        ssh(self.controller, f'kill $(cat {self.log}.pid) 2>/dev/null; rm -f {self.log}.out {self.log}.pid')


CHECKS = [
    ('private-address', 'tcp', 'F9-02 raw TCP through the assigned 127.100.x.x address'),
    ('explicit-listen', 'tcp', 'F9-02 / C125 pb access machine --port --listen, foreground owner'),
    ('https-port-host', 'http', 'F9-01 https://<port>.<m>.<local domain>'),
    ('https-host-port', 'http', 'F9-01 https://<m>.<local domain>:<port>'),
]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--controller', required=True)
    parser.add_argument('--target', required=True, help='machine alias as `pb machine list` shows it')
    parser.add_argument('--only', default='')
    args = parser.parse_args()
    only = set(filter(None, args.only.split(',')))
    machines = pb_json(args.controller, 'machine', 'list')['machines']
    found = [m for m in machines if m['alias'] == args.target and m.get('network_approved')]
    assert len(found) == 1, f'target {args.target} is not one approved machine'
    machine = found[0]
    origin = Origin(args.controller, args.target)
    failed = 0
    try:
        for name, kind, title in CHECKS:
            if only and name not in only:
                continue
            started = time.time()
            port = origin.ports[kind]
            command = ['python3', '-I', '-c', PROBE, name, machine['assigned_ip'], str(port), machine['alias'], machine['id'], origin.marker]
            result = ssh(args.controller, ' '.join(shlex.quote(c) for c in command), timeout=180)
            ok = result.returncode == 0 and 'PROBE_OK' in result.stdout
            failed += not ok
            detail = '' if ok else (result.stderr.strip().splitlines() or ['failed'])[-1][:200]
            print(f'{"PASS" if ok else "FAIL"} local-access:{name}:{args.controller}->{args.target} {time.time() - started:.1f}s  {title} {detail}', flush=True)
    finally:
        origin.close()
    sys.exit(1 if failed else 0)


if __name__ == '__main__':
    main()
