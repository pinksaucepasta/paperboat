#!/usr/bin/env python3
"""F14-05 metadata half, on a device (Linux, macOS or Windows): an official `pb` refuses altered signed update metadata.

Runs ON the device, as root (Windows: elevated), with an official install of pb and Python 3:
  tuf_metadata_tamper.py --pb PB --host RELEASE_HOST --ca CA.pem --cert SERVER.pem --key SERVER.key

The official client fetches `https://RELEASE_HOST/tuf/metadata/*.json` and trusts only the pinned root, over the system
TLS roots. The script puts a local mirror in front of that host: it adds the test CA to the system trust store, points
RELEASE_HOST at 127.0.0.1 in the hosts file, and serves every request by fetching the live repository (through the
address RELEASE_HOST had before) and, for one metadata document at a time, flipping one byte of its first signature.
Steps, each printed as one JSON line: control (nothing altered: `pb update check` and `download` succeed, so the mirror
path itself works), then timestamp, snapshot and targets altered one by one: `pb update check` and `pb update download`
must fail, `pb update status` must stay readable and the installed version unchanged. The hosts entry, the trust entry
and the mirror are removed on every exit path. Never prints keys or tokens.
"""
import argparse
import http.server
import json
import os
import platform
import re
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import urllib.request

SYSTEM = platform.system()
HOSTS = r'C:\Windows\System32\drivers\etc\hosts' if SYSTEM == 'Windows' else '/etc/hosts'
MARK = '# reo-tuf-tamper'
TAMPER = {'doc': None}


def run(argv, timeout=300):
    done = subprocess.run(argv, capture_output=True, text=True, timeout=timeout, stdin=subprocess.DEVNULL)
    return done.returncode, done.stdout + done.stderr


def trust(ca, add):
    if SYSTEM == 'Linux':
        target = '/usr/local/share/ca-certificates/reo-tuf-tamper.crt'
        if add:
            shutil.copyfile(ca, target)
        elif os.path.exists(target):
            os.remove(target)
        return run(['update-ca-certificates', '--fresh'])[0] == 0
    if SYSTEM == 'Darwin':
        if add:
            return run(['security', 'add-trusted-cert', '-d', '-r', 'trustRoot', '-k', '/Library/Keychains/System.keychain', ca])[0] == 0
        run(['security', 'remove-trusted-cert', '-d', ca])
        run(['security', 'delete-certificate', '-c', 'reo tuf tamper test ca', '/Library/Keychains/System.keychain'])
        return True
    if add:
        return run(['certutil', '-addstore', '-f', 'Root', ca])[0] == 0
    return run(['certutil', '-delstore', 'Root', 'reo tuf tamper test ca'])[0] == 0


def hosts(host, add):
    text = open(HOSTS).read() if os.path.exists(HOSTS) else ''
    text = ''.join(line for line in text.splitlines(True) if MARK not in line)
    if add:
        text += ('' if text.endswith('\n') or not text else '\n') + f'127.0.0.1 {host} {MARK}\n'
    open(HOSTS, 'w').write(text)
    if SYSTEM == 'Windows':
        run(['ipconfig', '/flushdns'])
    elif SYSTEM == 'Darwin':
        run(['dscacheutil', '-flushcache'])


def tamper_signature(body):
    """Flip one hex digit inside the first "sig":"..." value, keeping the document valid JSON."""
    found = re.search(rb'"sig":"([0-9a-f]+)"', body)
    digit = found.start(1)
    flipped = b'0' if body[digit:digit + 1] != b'0' else b'1'
    return body[:digit] + flipped + body[digit + 1:]


class Mirror(http.server.BaseHTTPRequestHandler):
    real_ip, host = '', ''

    def log_message(self, *args):
        pass

    def do_GET(self):
        connection = socket.create_connection((self.real_ip, 443), timeout=30)
        context = ssl.create_default_context()
        with context.wrap_socket(connection, server_hostname=self.host) as upstream:
            upstream.sendall(f'GET {self.path} HTTP/1.1\r\nHost: {self.host}\r\nAccept-Encoding: identity\r\nConnection: close\r\n\r\n'.encode())
            raw = b''
            while True:
                chunk = upstream.recv(65536)
                if not chunk:
                    break
                raw += chunk
        head, _, body = raw.partition(b'\r\n\r\n')
        status = int(head.split(b' ')[1])
        name = self.path.rsplit('/', 1)[-1]
        if status == 200 and TAMPER['doc'] and name == TAMPER['doc'] + '.json' and '/metadata/' in self.path:
            body = tamper_signature(body)
        self.send_response(status)
        self.send_header('Content-Type', 'application/json' if self.path.endswith('.json') else 'application/octet-stream')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def version(pb):
    return json.loads(run([pb, '--version', '--json'])[1])['data']['version']


def pb_json(pb, *arguments):
    code, out = run([pb, *arguments, '--json'], 400)
    try:
        return code, json.loads(out[out.index('{'):])
    except ValueError:
        return code, {'raw': out[-200:]}


def step(name, ok, **detail):
    print(json.dumps({'step': name, 'status': 'PASS' if ok else 'FAIL', **detail}), flush=True)
    return ok


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('pb', 'host', 'ca', 'cert', 'key'):
        parser.add_argument('--' + name, required=True)
    args = parser.parse_args()
    Mirror.host, Mirror.real_ip = args.host, socket.gethostbyname(args.host)
    installed = version(args.pb)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(args.cert, args.key)
    server = http.server.ThreadingHTTPServer(('127.0.0.1', 443), Mirror)
    server.socket = context.wrap_socket(server.socket, server_side=True)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    ok = True
    try:
        ok = step('trust-ca', trust(args.ca, True)) and ok
        hosts(args.host, True)
        for doc in (None, 'timestamp', 'snapshot', 'targets'):
            TAMPER['doc'] = doc
            check, download = pb_json(args.pb, 'update', 'check'), pb_json(args.pb, 'update', 'download')
            status = pb_json(args.pb, 'update', 'status')
            unchanged = version(args.pb) == installed
            if doc is None:
                ok = step('control-untampered', check[0] == 0 and download[0] == 0 and unchanged, check_rc=check[0], download_rc=download[0]) and ok
            else:
                refused = check[0] != 0 and download[0] != 0
                ok = step(f'tampered-{doc}-refused', refused and unchanged and status[0] == 0, check_rc=check[0], download_rc=download[0],
                          check_error=check[1].get('error', {}).get('code'), download_error=download[1].get('error', {}).get('code'),
                          version=installed) and ok
    finally:
        hosts(args.host, False)
        trust(args.ca, False)
        server.shutdown()
    print(json.dumps({'check': 'tuf-metadata-tamper', 'os': SYSTEM, 'status': 'PASS' if ok else 'FAIL'}), flush=True)
    sys.exit(0 if ok else 1)


if __name__ == '__main__':
    main()
