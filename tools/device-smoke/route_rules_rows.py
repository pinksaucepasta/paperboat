#!/usr/bin/env python3
"""F8-16: header and URL rules on a tunnel route, through the real edge.

A disposable enrolled machine (--machine-cmd, a command prefix such as
`ssh pbvm4 sudo -n docker exec -i -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 small-t1`)
runs an echo origin that answers with the path and headers it received and counts
its requests, and a durable tunnel in front of it. Visitor requests come from this host with curl.
Checks: rules set with `pb tunnel route update` reach the edge within RULE_BOUND seconds and then
  - a request header rule and a path-prefix rewrite change what the origin sees (query kept, /apix untouched, %2F stays one segment),
  - a response header rule adds a header and removes the origin's,
  - /old answers 308 with the Location of the new path (rest and query kept) and never reaches the origin,
  - the edge's own X-Forwarded-For stays the visitor's real address whatever the visitor or a rule says,
  - the CLI refuses a rule on an edge header and a redirect that loops, nothing changes,
  - `route list --json` shows the rules, --clear-redirects ends the redirect only.
Everything it creates it removes again. Usage: route_rules_rows.py --machine-cmd '<prefix>' [--pb /home/pbt/.local/bin/pb]
"""
import argparse
import json
import os
import shlex
import subprocess
import sys
import time
from pathlib import Path

from machine_ctl import Failure, Machine, remote, run

# A rule reaches the edge with the next route snapshot (seconds), the same bound as the ip-rules check.
RULE_BOUND = 15
ORIGIN_PORT = '9318'

ECHO_ORIGIN = r'''
import http.server, json
COUNT = [0]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/__count":
            body = json.dumps({"count": COUNT[0]}).encode()
        else:
            COUNT[0] += 1
            body = json.dumps({"path": self.path, "headers": {k.lower(): v for k, v in self.headers.items()}}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("X-Secret", "s3")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a):
        pass
http.server.ThreadingHTTPServer(("127.0.0.1", __PORT__), H).serve_forever()
'''


def curl(url, *headers):
    """(status, response headers lowercased, body) of one visitor request from this host; no redirect is followed."""
    argv = ['curl', '-sS', '-m', '25', '-D', '-', '-o', '-', '--path-as-is']
    for header in headers:
        argv += ['-H', header]
    code, out, err = run(argv + [url], 60)
    out = out.replace('\r\n', '\n')  # text mode already did on some paths: normalize either way
    head, _, body = out.partition('\n\n')
    while body.startswith('HTTP/'):  # a 100-continue or proxy block before the real head
        head, _, body = body.partition('\n\n')
    lines = head.split('\n')
    status = int(lines[0].split()[1]) if lines and lines[0].startswith('HTTP/') else 0
    parsed = {}
    for line in lines[1:]:
        name, _, value = line.partition(':')
        parsed.setdefault(name.strip().lower(), value.strip())
    return status, parsed, body


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--machine-cmd', default='', help='command prefix that runs a command on the machine (not with --windows)')
    parser.add_argument('--windows', action='store_true', help='the machine is the Windows device selected by SMOKE_WINDOWS_SSH and PB_SMOKE_WINDOWS_PB')
    parser.add_argument('--pb', default='/home/pbt/.local/bin/pb')
    parser.add_argument('--output', default='')
    parser.add_argument('--wait', type=int, default=RULE_BOUND, help='seconds a rule may take to reach the edge')
    args = parser.parse_args()
    try:
        machine = Machine(args.machine_cmd, args.pb, args.windows)
    except Failure as error:
        parser.error(str(error))
    name = 'tor-rr-%d-%s' % (int(time.time()), os.urandom(2).hex())  # tunnel names are account-wide
    steps, cleanup, latencies = [], [], []
    state = {}

    pb = machine.pb

    def good(*arguments, timeout=180):
        code, value = pb(*arguments, '--json', timeout=timeout)
        if code != 0:
            raise Failure('%s: %s' % (' '.join(arguments), str(value)[:300]))
        return value

    def refused(*arguments):
        code, value = pb(*arguments, '--json')
        if code == 0:
            raise Failure('%s succeeded but must be refused' % ' '.join(arguments))
        return json.dumps(value)

    def origin_count():
        code, out, _ = machine.run_command(['curl', '-sS', '-m', '10', 'http://127.0.0.1:%s/__count' % ORIGIN_PORT], 30)
        return json.loads(out)['count']

    def seen(path, *headers):
        status, head, body = curl(state['url'] + path, *headers)
        if status != 200:
            raise Failure('GET %s answered HTTP %d' % (path, status))
        return head, json.loads(body)

    def eventually(label, check, seconds=None):
        deadline, started, last = time.time() + (seconds or args.wait), time.time(), None
        while time.time() < deadline:
            try:
                value = check()
                latencies.append(round(time.time() - started, 1))
                return value
            except Failure as error:
                last = error
            time.sleep(1)
        raise Failure('%s: not within %ds (%s)' % (label, seconds or args.wait, last))

    def step(label, function):
        started = time.time()
        try:
            function()
            steps.append({'step': label, 'status': 'PASS', 'seconds': round(time.time() - started, 1)})
        except Exception as error:  # noqa: BLE001 - keep going so the cleanup runs and the report shows every failure
            steps.append({'step': label, 'status': 'FAIL', 'seconds': round(time.time() - started, 1), 'detail': str(error)[:400]})

    def update(*flags):
        return good('tunnel', 'route', 'update', state['id'], state['route'], *flags)

    def baseline():
        eventually('tunnel serves', lambda: seen('/plain'), 180)
        if good('tunnel', 'route', 'list', state['id'])['items'][0].get('rewrite_rules') not in (None, {}):
            raise Failure('a new route starts with rules')

    def set_rules():
        update('--request-header', 'set:X-Env=prod', '--request-header', 'add:X-Trace=a', '--response-header', 'set:X-Served-By=paperboat',
               '--response-header', 'remove:X-Secret', '--rewrite-path', '/api -> /v1', '--redirect', '/old -> 308:/new')
        shown = good('tunnel', 'route', 'list', state['id'])['items'][0].get('rewrite_rules') or {}
        if len(shown.get('request_headers', [])) != 2 or len(shown.get('response_headers', [])) != 2 or shown.get('path_rewrite', {}).get('to') != '/v1' or len(shown.get('redirects', [])) != 1:
            raise Failure('route list does not show the rules: %s' % json.dumps(shown))

    def request_side():
        def check():
            head, body = seen('/api/users?x=1')
            if body['path'] != '/v1/users?x=1' or body['headers'].get('x-env') != 'prod' or body['headers'].get('x-trace') != 'a':
                raise Failure('origin saw %s' % json.dumps(body)[:200])
        eventually('request rules', check)
        if seen('/apix')[1]['path'] != '/apix':
            raise Failure('/apix was rewritten: the prefix match ignores segment boundaries')
        if seen('/api/files/a%2Fb')[1]['path'] != '/v1/files/a%2Fb':
            raise Failure('an encoded slash did not stay one segment: %s' % seen('/api/files/a%2Fb')[1]['path'])

    def response_side():
        head, _ = seen('/api/users')
        if head.get('x-served-by') != 'paperboat' or 'x-secret' in head:
            raise Failure('response headers: x-served-by=%r x-secret=%r' % (head.get('x-served-by'), head.get('x-secret')))

    def redirect():
        before = origin_count()
        status, head, _ = curl(state['url'] + '/old/page?a=1')
        if status != 308 or head.get('location') != '/new/page?a=1':
            raise Failure('/old/page?a=1 answered %d Location %r' % (status, head.get('location')))
        if origin_count() != before:
            raise Failure('the redirect reached the origin')

    def edge_wins():
        head, body = seen('/api/x', 'X-Forwarded-For: 6.6.6.6', 'Paperboat-Visitor-Assertion: forged')
        xff = body['headers'].get('x-forwarded-for', '')
        if '6.6.6.6' in xff or not xff or 'paperboat-visitor-assertion' in body['headers']:
            raise Failure('the visitor spoofed an edge header: xff=%r' % xff)

    def cli_refusals():
        refused('tunnel', 'route', 'update', state['id'], state['route'], '--request-header', 'set:X-Forwarded-For=1.2.3.4')
        refused('tunnel', 'route', 'update', state['id'], state['route'], '--response-header', 'set:Strict-Transport-Security=max-age=0')
        refused('tunnel', 'route', 'update', state['id'], state['route'], '--redirect', '/loop -> /loop/x')
        shown = good('tunnel', 'route', 'list', state['id'])['items'][0].get('rewrite_rules') or {}
        if len(shown.get('request_headers', [])) != 2 or len(shown.get('redirects', [])) != 1:
            raise Failure('a refused update changed the rules')

    def clear_redirects():
        update('--clear-redirects')

        def check():
            status, head, _ = curl(state['url'] + '/old/page')
            if status == 308:
                raise Failure('/old still redirects')
            if seen('/api/users')[1]['path'] != '/v1/users':
                raise Failure('--clear-redirects also removed the path rewrite')
        eventually('redirect cleared', check)

    try:
        cleanup.append(machine.start_origin(ORIGIN_PORT, 'rr-origin', source=ECHO_ORIGIN))
        created = good('tunnel', 'create', name, '--port', ORIGIN_PORT, '--duration', '30m', '--wait', '--timeout', '90s', timeout=200)
        state['id'], state['url'] = created['tunnel']['id'], created['tunnel']['stable_endpoint'].rstrip('/')

        def delete():
            code, value = pb('tunnel', 'delete', state['id'], '--json')
            token = value.get('confirmation_token') if isinstance(value, dict) else None
            if token:
                pb('tunnel', 'delete', state['id'], '--confirm', token, '--json')
        cleanup.append(delete)
        state['route'] = good('tunnel', 'route', 'list', state['id'])['items'][0]['id']
        step('baseline: the tunnel serves and its route has no rules', baseline)
        if steps[-1]['status'] != 'PASS':
            raise Failure('the baseline failed')
        for label, function in (('set request/response header rules, a path rewrite and a redirect; route list shows them', set_rules),
                                ('request side: header rules, /api -> /v1 with the query, /apix and %2F untouched', request_side),
                                ('response side: a header added, the origin\'s removed', response_side),
                                ('/old answers 308 to /new with the rest and query and never reaches the origin', redirect),
                                ('the edge\'s X-Forwarded-For and the visitor assertion cannot be spoofed by the visitor', edge_wins),
                                ('the CLI refuses an edge header, a security header and a looping redirect; nothing changes', cli_refusals),
                                ('--clear-redirects ends the redirect only', clear_redirects)):
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
    report = {'check': 'route-rules-rows', 'row': 'F8-16', 'status': 'PASS' if ok else 'FAIL', 'bound_seconds': args.wait,
              'seconds_until_each_rule_applied': latencies, 'steps': steps}
    print(json.dumps(report, indent=1))
    if args.output:
        Path(args.output).write_text(json.dumps(report, indent=1))
    return 0 if ok else 1


if __name__ == '__main__':
    sys.exit(main())
