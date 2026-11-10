#!/usr/bin/env python3
"""C126 with a Windows client: a Unix peer holds a private tunnel with a tcp_private echo route, pb.exe on the device
accesses it by route and by tunnel id (tunnel_private_access.py does the access, echo and listener checks).

  PB_SMOKE_WINDOWS_PB=... tunnel_private_access_windows.py PEER
"""
import base64
import json
import sys
import time
import uuid

from pairing_lifecycle import command_result, invoke
from tunnel_private_access import error_case, one_session
import winhost

# Runs on the peer through `pb exec`: an echo origin, a private tunnel and a tcp_private route to it, held until killed.
PEER_SETUP = r'''
import json, os, socketserver, subprocess, sys, threading
pb = os.path.expanduser('~/.local/bin/pb')
nonce = sys.argv[1]
class Echo(socketserver.BaseRequestHandler):
    def handle(self):
        while True:
            data = self.request.recv(65536)
            if not data:
                break
            self.request.sendall(data)
socketserver.ThreadingTCPServer.daemon_threads = True
server = socketserver.ThreadingTCPServer(('127.0.0.1', 0), Echo)
port = server.server_address[1]
threading.Thread(target=server.serve_forever, daemon=True).start()
def run(*args):
    out = json.loads(subprocess.run([pb, *args, '--wait', '--timeout', '90s', '--json'], capture_output=True, text=True, check=True).stdout)
    return out.get('data', out)
tunnel = run('tunnel', 'create', 'win-tun-' + nonce, '--port', str(port), '--private')['tunnel']['id']
route = run('tunnel', 'route', 'add', tunnel, '--name', 'echo', '--protocol', 'tcp_private', '--to', 'tcp://127.0.0.1:%d' % port)['route']['id']
print(tunnel, route, flush=True)
threading.Event().wait()
'''


def delete_tunnel(pb, tunnel):
    token = invoke(pb, 'tunnel', 'delete', tunnel, '--json')['confirmation_token']
    invoke(pb, 'tunnel', 'delete', tunnel, '--confirm', token, '--json')


def main():
    if not winhost.device():
        raise SystemExit('PB_SMOKE_WINDOWS_PB must name the device pb.exe')
    peer, pb, nonce = sys.argv[1], winhost.pb_path(), uuid.uuid4().hex[:8]
    code = base64.b64encode(PEER_SETUP.encode()).decode()
    origin = winhost.job(pb, ['exec', peer, '--timeout', '600s', '--', 'python3', '-c',
                              "import base64;exec(base64.b64decode('%s'))" % code, nonce])
    tunnel = None
    try:
        deadline = time.monotonic() + 240
        while b'\n' not in origin.stdout():
            if origin.poll() is not None or time.monotonic() > deadline:
                raise RuntimeError('the peer did not publish its tunnel: ' + origin.stderr()[-300:].decode(errors='replace'))
            time.sleep(1)
        tunnel, route = origin.stdout().split(b'\n')[0].decode().split()[:2]
        errors = [error_case(pb, ['access', 'tunnel', '--json']),
                  error_case(pb, ['access', 'tunnel', route, '--listen', '0.0.0.0:0', '--json']),
                  error_case(pb, ['access', 'tunnel', 'missing-' + nonce, '--json'])]
        helped = command_result(pb, 'access', 'tunnel', '--help', timeout=30)
        help_ok = helped.returncode == 0 and 'Usage:' in helped.stdout and 'loopback' in helped.stdout
        sessions = [one_session(pb, selector, tunnel, route, nonce) for selector in (route, tunnel)]
        passed = help_ok and all(x['status'] == 'PASS' for x in [*errors, *sessions])
        print(json.dumps({'check': 'tunnel-private-access', 'row': 'C126', 'status': 'PASS' if passed else 'FAIL',
                          'help': help_ok, 'errors': errors, 'sessions': sessions}))
    finally:
        try:
            if tunnel:
                delete_tunnel(pb, tunnel)
        finally:
            origin.kill()
            origin.close()
    raise SystemExit(0 if passed else 1)


if __name__ == '__main__':
    main()
