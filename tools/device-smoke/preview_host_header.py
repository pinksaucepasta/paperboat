#!/usr/bin/env python3
"""`pb preview --host-header`: the origin receives the Host you name, through the real edge.

A disposable enrolled machine (--machine-cmd, a command prefix such as
`ssh pbvm4 sudo -n docker exec -i -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 small-t1`) runs an origin that, like a
dev server with a host allow-list (Vite, Django ALLOWED_HOSTS), answers 403 to any Host but ALLOWED_HOST.
Checks: a plain preview is refused by the origin (403, the visitor's own host reaches it); the same origin behind
`--host-header ALLOWED_HOST` answers 200 and sees that Host; a malformed value, a value with a port and a value on a unix
target are refused before anything starts; `preview status --json` names the header.
Everything it creates it removes again. Usage: preview_host_header.py --machine-cmd '<prefix>' [--pb /home/pbt/.local/bin/pb]
"""
import argparse
import json
import shlex
import sys
import time
from pathlib import Path

from route_rules_rows import curl, remote, run

ALLOWED_HOST = 'allowed.example.test'
ORIGIN_PORT = '9319'
ORIGIN = r'''
import http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        host = self.headers.get("Host", "")
        ok = host == "__HOST__"
        body = (("ok host=" if ok else "blocked host=") + host).encode()
        self.send_response(200 if ok else 403)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a):
        pass
http.server.ThreadingHTTPServer(("127.0.0.1", __PORT__), H).serve_forever()
'''


class Failure(Exception):
    pass


def find_url(value):
    if isinstance(value, str):
        return value if value.startswith('https://') else None
    for child in (value.values() if isinstance(value, dict) else value if isinstance(value, list) else []):
        found = find_url(child)
        if found:
            return found
    return None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--machine-cmd', required=True)
    parser.add_argument('--pb', default='/home/pbt/.local/bin/pb')
    parser.add_argument('--output', default='')
    args = parser.parse_args()
    machine = shlex.split(args.machine_cmd)
    steps, cleanup = [], []

    def pb(*arguments, timeout=180):
        code, out, err = run(remote(machine, [args.pb, *arguments]), timeout)
        try:
            value = json.loads(out)
            value = value.get('data', value) if isinstance(value, dict) else value
        except ValueError:
            value = out.strip() or err.strip()
        return code, value

    def start(*flags):
        code, value = pb('preview', ORIGIN_PORT, '--background', '--ttl', '30m', *flags, '--json', timeout=200)
        if code != 0:
            raise Failure('preview %s: %s' % (' '.join(flags), str(value)[:300]))
        ident = value.get('id') or (value.get('preview') or {}).get('id')
        url = find_url(value)
        if not ident or not url:
            raise Failure('preview receipt lacks an id or URL: ' + json.dumps(value)[:200])

        def stop():
            pb('preview', 'stop', ident, '--json')
            pb('preview', 'delete', ident, '--json')
        cleanup.append(stop)
        return ident, url

    def get(url):
        deadline, last = time.time() + 60, None
        while time.time() < deadline:
            last = curl(url)
            if last[0] in (200, 403):
                return last
            time.sleep(2)
        raise Failure('no origin answer from %s (last %s)' % (url, last and last[0]))

    def step(label, function):
        started = time.time()
        try:
            function()
            steps.append({'step': label, 'status': 'PASS', 'seconds': round(time.time() - started, 1)})
        except Exception as error:  # noqa: BLE001
            steps.append({'step': label, 'status': 'FAIL', 'seconds': round(time.time() - started, 1), 'detail': str(error)[:400]})

    ids = {}

    def plain():
        ids['plain'], url = start()
        status, _, body = get(url)
        if status != 403 or 'blocked host=' not in body or ALLOWED_HOST in body:
            raise Failure('plain preview: HTTP %d %r' % (status, body[:80]))

    def named():
        ids['named'], url = start('--host-header', ALLOWED_HOST)
        status, _, body = get(url)
        if status != 200 or body != 'ok host=' + ALLOWED_HOST:
            raise Failure('named preview: HTTP %d %r' % (status, body[:80]))

    def shown():
        code, value = pb('preview', 'status', ids['named'], '--json')
        if code != 0 or ALLOWED_HOST not in json.dumps(value):
            raise Failure('preview status does not name the header: %s' % json.dumps(value)[:200])

    def refusals():
        for bad in ('bad host', ALLOWED_HOST + ':8080', 'a/b', 'user@host'):
            code, value = pb('preview', ORIGIN_PORT, '--background', '--host-header', bad, '--json')
            if code == 0:
                raise Failure('--host-header %r was accepted' % bad)
        code, value = pb('preview', 'unix:/tmp/none.sock', '--background', '--host-header', ALLOWED_HOST, '--json')
        if code == 0:
            raise Failure('--host-header on a unix target was accepted')

    try:
        code, _, err = run(remote(machine, ['sh', '-c', 'cat > /tmp/ph-origin.py && (nohup python3 /tmp/ph-origin.py >/dev/null 2>&1 &)']), 60,
                           input=ORIGIN.replace('__HOST__', ALLOWED_HOST).replace('__PORT__', ORIGIN_PORT))
        if code != 0:
            raise Failure('origin did not start: ' + err[:200])
        cleanup.append(lambda: run(remote(machine, ['sh', '-c', 'pkill -f /tmp/ph-origin.py; rm -f /tmp/ph-origin.py']), 30))
        for label, function in (('a plain preview: the origin refuses the visitor\'s Host (403)', plain),
                                ('--host-header: the origin sees the named Host and answers 200', named),
                                ('preview status names the header', shown),
                                ('a malformed value, a port, a path, credentials and a unix target are refused', refusals)):
            step(label, function)
    except Exception as error:  # noqa: BLE001
        steps.append({'step': 'fixture', 'status': 'FAIL', 'detail': str(error)[:400]})
    finally:
        while cleanup:
            try:
                cleanup.pop()()
            except Exception as error:  # noqa: BLE001
                print('cleanup failed:', str(error)[:200], file=sys.stderr)
    ok = bool(steps) and all(s['status'] == 'PASS' for s in steps)
    report = {'check': 'preview-host-header', 'status': 'PASS' if ok else 'FAIL', 'steps': steps}
    print(json.dumps(report, indent=1))
    if args.output:
        Path(args.output).write_text(json.dumps(report, indent=1))
    return 0 if ok else 1


if __name__ == '__main__':
    sys.exit(main())
