#!/usr/bin/env python3
"""F27-04 / F27-05: run on a Linux HOST machine with sudo -n, or from the controller for a Windows host
(PB_SMOKE_WINDOWS_PB set, python on the device, see winorigin.py); VIEWER is another machine of the account.

  python3 https_front_desk.py VIEWER [--high-ports]

The host (the machine guard service paperboat-machineguard) runs a web server the way Traefik/Caddy/Coolify do: TLS on 0.0.0.0:443 (self-signed) and a plain 0.0.0.0:80 that
redirects every request to https://<same host>/. Both ports are held, so the machine's own gateway moves to free ports.
--high-ports (any Unix host, no sudo: macOS too, or a Windows host) serves the same pair on free ports >= 1024
instead of 80/443 and skips the guard, policy and nft steps. A Windows host stops IIS (W3SVC/WAS) and starts the
web server on 80/443; own address is checked in both modes. Checked: with ports 80 and 443 listed by a tunnel policy and the front desk 443/https the viewer opens an unmatched name and `443.<host>` over TLS (no redirect loop);
the host opens its own address; with the front desk 80/http the viewer gets the explanation page (502,
X-Paperboat-Error: origin_redirect_loop), not a loop; the host's nft table redirects the held ports.
"""
import argparse
import contextlib
import json
import shutil
import subprocess
import sys
import tempfile
import time
import uuid
from pathlib import Path

import port_names
import winhost
import winorigin
from install_lifecycle import registration
from port_names import BINARY, command_result, fetch, pb, pb_ok, version

HOLDER = r'''
import http.server, ssl, sys, threading
class Tls(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = b'tls443'
        self.send_response(200); self.send_header('Content-Length', str(len(body))); self.end_headers(); self.wfile.write(body)
    log_message = lambda *a: None
class Plain(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(301); self.send_header('Location', 'https://%s%s' % (self.headers.get('Host', ''), self.path))
        self.send_header('Content-Length', '0'); self.end_headers()
    log_message = lambda *a: None
tls = http.server.ThreadingHTTPServer(('0.0.0.0', int(sys.argv[3])), Tls)  # port 0 = any free port
context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER); context.load_cert_chain(sys.argv[1], sys.argv[2])
tls.socket = context.wrap_socket(tls.socket, server_side=True)
threading.Thread(target=tls.serve_forever, daemon=True).start()
plain = http.server.ThreadingHTTPServer(('0.0.0.0', int(sys.argv[4])), Plain)
print('ready', tls.server_port, plain.server_port, flush=True)
plain.serve_forever()
'''


def guard(action):
    subprocess.run(['sudo', '-n', 'systemctl', action, 'paperboat-machineguard.service'], check=True)


def windows_holder(stack, device, root, high_ports):
    """The web server on a Windows host. For 80/443 IIS steps aside first (HTTP.sys holds port 80 exclusively); the
    daemon keeps running: its browser gateway sits on its own loopback address and the web server takes the wildcard."""
    if not high_ports:
        for name in ('W3SVC', 'WAS'):
            if device.services(name):
                device.service('stop', name)
    files = {name: Path(root, name).read_bytes() for name in ('c', 'k')}
    return stack.enter_context(winorigin.origin(device, HOLDER, '{dir}\\c', '{dir}\\k', *(('0', '0') if high_ports else ('443', '80')), files=files))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('viewer')
    parser.add_argument('--high-ports', action='store_true')
    args = parser.parse_args()
    before = registration()
    machines = pb_ok('machine', 'list', '--all', '--json')
    machines = machines.get('machines', machines.get('items', [])) if isinstance(machines, dict) else machines
    host = next(m['alias'] for m in machines if m['id'] == before['machine_id'])
    domain = pb_ok('config', 'local-access', 'show', '--json')['domain']
    name = 'zz' + uuid.uuid4().hex[:6]
    root = tempfile.mkdtemp(prefix='pb-https-front-desk-')
    target = winhost.device()
    steps, failure, holder, policies, stack = {}, None, None, [], contextlib.ExitStack()

    def step(label, action):
        try:
            action()
            steps[label] = 'PASS'
        except Exception as error:
            steps[label] = 'FAIL: ' + str(error)[:300]
            raise

    try:
        subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1', '-subj', '/CN=held',
                        '-keyout', root + '/k', '-out', root + '/c'], check=True, capture_output=True)
        if target:
            line = windows_holder(stack, target, root, args.high_ports)
        else:
            Path(root, 'holder.py').write_text(HOLDER)
            if not args.high_ports:
                guard('stop')  # the web server was there first: the guard starts after it and finds both ports taken
            holder = subprocess.Popen(([] if args.high_ports else ['sudo', '-n']) + [
                sys.executable, root + '/holder.py', root + '/c', root + '/k', *(('0', '0') if args.high_ports else ('443', '80'))],
                stdout=subprocess.PIPE, text=True)
            line = holder.stdout.readline()
        ready = line.split()
        assert ready[:1] == ['ready'], 'the web server could not take its ports'
        secure, plain = ready[1:3]

        def held():
            if args.high_ports:
                return
            if target:
                return
            guard('start')
            time.sleep(15)  # the guard moves its gateway when the bind fails
            rules = subprocess.run(['sudo', '-n', 'nft', 'list', 'ruleset'], capture_output=True, text=True).stdout
            assert 'dnat' in rules, 'no nft redirect for the held ports'
        step('web_server_holds_80_443', held)
        # Names open listed ports only, and the automatic list stops at 1024: 80 and 443 are listed by a policy.
        for port in () if args.high_ports else ('80', '443'):
            policies.append(pb_ok('tunnel', 'policy', 'allow', host, port, '--json')['id'])
        pb_ok('port', 'front-desk', host, secure, 'https')
        step('front_desk_https', lambda: fetch(args.viewer, '%s.%s.%s' % (name, host, domain), 'tls443'))
        if not args.high_ports:  # only the numeric label 443 is https; other numeric labels speak plain http
            step('port_443_label', lambda: fetch(args.viewer, '443.%s.%s' % (host, domain), 'tls443'))

        def own():
            argv = ['curl.exe' if target else 'curl', '-fsS', '--noproxy', '*', '--max-time', '15', 'https://%s.%s.%s/who' % (name, host, domain)]
            result = target.run(argv) if target else subprocess.run(argv, capture_output=True, text=True)
            assert result.returncode == 0 and result.stdout.strip() == 'tls443', 'own address: ' + (result.stderr[-200:] or result.stdout)
        step('host_opens_own_address', own)
        pb_ok('port', 'front-desk', host, plain, 'http')
        step('redirect_gets_explanation', lambda: fetch(
            args.viewer, '%s.%s.%s' % (name, host, domain),
            lambda body: ' 502' in body.splitlines()[0] and 'origin_redirect_loop' in body.lower(), path='/who', curl=('-sS', '-i')))
    except Exception as error:
        failure = error
    finally:
        pb('port', 'front-desk', host, '80', 'http', '--json')
        for policy in policies:
            pb('tunnel', 'policy', 'revoke', policy, '--json')
        if holder:
            holder.terminate()
            try:
                holder.wait(timeout=10)
            except subprocess.TimeoutExpired:
                subprocess.run(['sudo', '-n', 'kill', '-9', str(holder.pid)])
        if not args.high_ports and not target:
            subprocess.run(['sudo', '-n', 'systemctl', 'restart', 'paperboat-machineguard.service'])
        stack.close()
        shutil.rmtree(root, ignore_errors=True)
    ok = failure is None and registration() == before
    print(json.dumps({'check': 'https-front-desk', 'target': args.viewer, 'status': 'PASS' if ok else 'FAIL', 'steps': steps,
                      'host_version': version(), 'enrollment_unchanged': registration() == before}))
    sys.exit(0 if ok else 1)


if __name__ == '__main__':
    main()
