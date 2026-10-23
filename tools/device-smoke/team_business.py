#!/usr/bin/env python3
"""F19 live check: team roles, audit log search/export and audit log streaming, run from the control host.

CMD runs commands in a paired Linux machine whose pb is signed in as the owner of TEAM (a Team-plan
team with another active member). The check changes that member's role to auditor and billing and
back (F19-01), searches the audit log with every filter and pages it (F19-07/08), exports it as CSV
and JSON (F19-09), and streams it (F19-10): a receiver on the machine, published with an ephemeral
`pb tunnel`, gets signed NDJSON with the custom header; a refused batch is retried; and an
S3-compatible store (--s3-origin, e.g. versitygw, keys in ~/.f19-s3ak and ~/.f19-s3sk on the machine)
gets gzip NDJSON objects through SigV4. Stream setup uses a one-day API token that is revoked at the
end. Secrets (token, signing secret, S3 keys) stay on the machine and are never printed.

Usage: team_business.py --team paperboat-smoke-paid \
         --cmd 'ssh hp docker exec -i -u tester -e XDG_RUNTIME_DIR=/run/user/1001 -w /home/tester lanes-f19' \
         --s3-origin 10.0.0.12:7070 --s3-list-cmd 'ssh hp docker exec -i lanes-s3'
"""
import argparse
import csv
import io
import json
import shlex
import subprocess
import sys
import time

# Runs on the machine: the API client (token from ~/.f19tok), the receiver and the secret handling.
HELPER = r'''
import hashlib, hmac, json, os, subprocess, sys, urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer
H = os.path.expanduser('~')
def rd(n): return open(os.path.join(H, n)).read().strip()
def wr(n, v):
    fd = os.open(os.path.join(H, n), os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    os.write(fd, v.encode()); os.close(fd)
cmd = sys.argv[1]
if cmd == 'token':
    # token PB NAME WORKSPACE [FILE [SCOPE]]: a one-day API token, its secret only in FILE.
    extra = ['--scope', sys.argv[6]] if len(sys.argv) > 6 else []
    out = subprocess.run([sys.argv[2], 'api-token', 'create', sys.argv[3], '--expires', '1d', '--workspace', sys.argv[4], '--json'] + extra, capture_output=True, text=True)
    d = json.loads(out.stdout); d = d.get('data', d)
    wr(sys.argv[5] if len(sys.argv) > 5 else '.f19tok', d['secret']); wr('.f19hdr', os.urandom(16).hex())
    print(d['api_token']['id'])
elif cmd == 'api':
    method, path = sys.argv[2], sys.argv[3]
    body = json.loads(sys.argv[4]) if len(sys.argv) > 4 else None
    if body:
        for k, v in list(body.items()):
            if isinstance(v, str) and v.startswith('@'):
                body[k] = rd(v[1:])
    req = urllib.request.Request(sys.argv[5] if len(sys.argv) > 5 else rd('.f19api') + path, method=method,
        data=json.dumps(body).encode() if body is not None else None,
        headers={'Authorization': 'Bearer ' + rd(os.environ.get('F19TOK', '.f19tok')), 'Content-Type': 'application/json'})
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            status, raw = r.status, r.read()
    except urllib.error.HTTPError as e:
        status, raw = e.code, e.read()
    try:
        d = json.loads(raw or b'null')
    except ValueError:
        d = raw.decode(errors='replace')[:300]
    data = d.get('data', d) if isinstance(d, dict) else d
    if isinstance(data, dict) and data.get('signing_secret'):
        wr('.f19sig', data['signing_secret']); data['signing_secret'] = '<stored>'
    print(json.dumps({'status': status, 'data': data}))
elif cmd == 'forward':
    # A local TCP forwarder: ephemeral tunnels publish local ports only.
    import socket, threading
    wr('.f19forward.pid', str(os.getpid()))
    host, port = sys.argv[3].rsplit(':', 1)
    def pipe(a, b):
        try:
            while True:
                d = a.recv(65536)
                if not d: break
                b.sendall(d)
        except OSError: pass
        finally:
            a.close(); b.close()
    ls = socket.create_server(('127.0.0.1', int(sys.argv[2])))
    while True:
        c, _ = ls.accept()
        u = socket.create_connection((host, int(port)))
        threading.Thread(target=pipe, args=(c, u), daemon=True).start()
        threading.Thread(target=pipe, args=(u, c), daemon=True).start()
elif cmd == 'serve':
    wr('.f19serve.pid', str(os.getpid()))
    log = os.path.join(H, 'f19recv.jsonl')
    class R(BaseHTTPRequestHandler):
        def do_POST(self):
            body = self.rfile.read(int(self.headers.get('Content-Length', 0)))
            if os.path.exists(os.path.join(H, 'f19recv.fail')):
                self.send_response(503); self.end_headers(); return
            sig = dict(p.split('=', 1) for p in self.headers.get('Pb-Signature', '').split(',') if '=' in p)
            want = hmac.new(rd('.f19sig').encode(), (sig.get('t', '') + '.').encode() + body, hashlib.sha256).hexdigest()
            events = [json.loads(l) for l in body.decode().splitlines() if l.strip()]
            rec = {'sig_ok': hmac.compare_digest(want, sig.get('v1', '')), 'header_ok': self.headers.get('X-F19-Check') == rd('.f19hdr'),
                   'type': self.headers.get('Content-Type'), 'events': [[e['id'], e['action'], e.get('result')] for e in events]}
            with open(log, 'a') as f:
                f.write(json.dumps(rec) + '\n')
            self.send_response(204); self.end_headers()
        def log_message(self, *a): pass
    HTTPServer(('127.0.0.1', int(sys.argv[2])), R).serve_forever()
'''

CSV_COLUMNS = ['id', 'occurred_at', 'workspace', 'action', 'result', 'reason', 'actor_kind', 'actor_id', 'actor_email',
               'actor_token_id', 'target_kind', 'target_id', 'target_name', 'machine_id', 'machine_name', 'ip',
               'user_agent', 'request_id', 'details']


class Check:
    def __init__(self, a):
        self.a, self.fails = a, []

    def run(self, args, timeout=180, stdin=None):
        remote = ' '.join(shlex.quote(x) for x in args)
        # CMD goes through ssh, whose remote shell splits the line once more: quote the command for it.
        done = subprocess.run(shlex.split(self.a.cmd) + ['bash -lc ' + shlex.quote(remote)], capture_output=True, text=True,
                              timeout=timeout, input=stdin)
        return done.returncode, done.stdout, done.stderr

    def pb(self, *args, **kw):
        """Runs pb; a response lost on the way back ("Paperboat is unreachable") is retried twice,
        since this check is about F19 behaviour, not the machine's network."""
        for attempt in range(3):
            rc, out, err = self.run([self.a.pb] + list(args), **kw)
            if rc == 0 or 'is unreachable' not in out + err:
                break
            print('RETRY pb ' + ' '.join(args[:3]) + ': unreachable')
            time.sleep(3)
        return rc, out, err

    def pbj(self, *args):
        rc, out, err = self.pb(*args, '--json')
        try:
            d = json.loads(out)
            return d.get('data', d) if isinstance(d, dict) else d
        except ValueError:
            return {'error': (out + err)[-300:]}

    def api(self, method, path, body=None, url=None):
        args = ['python3', '.f19helper.py', 'api', method, path] + ([json.dumps(body)] if body is not None or url else [])
        if url:
            args.append(url)
        rc, out, err = self.run(args)
        try:
            return json.loads(out)
        except ValueError:
            return {'status': 0, 'data': (out + err)[-300:]}

    def step(self, name, ok, detail=''):
        print(('PASS ' if ok else 'FAIL ') + name + ('' if ok else ': ' + str(detail).strip()[-400:]))
        if not ok:
            self.fails.append(name)
        return ok

    def activity(self, *filters):
        d = self.pbj('team', 'activity', self.a.team, *filters)
        return d.get('items') or [] if isinstance(d, dict) else [], d


def roles(c, team):
    """F19-01: the owner changes another member's role to auditor, billing and back."""
    t = c.pbj('team', 'get', team)
    owner = t.get('owner_account')
    others = [m for m in t.get('members', []) if m['active'] and m['account_id'] != owner]
    if not c.step('the team has another active member', bool(others), t):
        return None
    member, original = others[0]['account_id'], others[0]['role']
    for role in ['auditor', 'billing', original]:
        now, out, err = [], '', ''
        for attempt in range(3):  # a retried change can meet a newer generation: read it again
            gen = str(c.pbj('team', 'get', team).get('generation'))
            rc, out, err = c.pb('team', 'role', team, member, role, '--generation', gen)
            now = [m['role'] for m in c.pbj('team', 'get', team).get('members', []) if m['account_id'] == member]
            if now == [role]:
                break
        c.step('pb team role sets ' + role, now == [role], out + err)
    return member


def identity(c, team):
    """F19-14 (before the WorkOS dashboard steps): SSO and directory status read the team's state."""
    sso = c.pbj('team', 'sso', 'status', team)
    c.step('pb team sso status reads eligibility and SSO state', isinstance(sso, dict) and sso.get('eligible') is True
           and sso.get('seats', 0) >= sso.get('min_seats', 1) and 'required' in (sso.get('sso') or {}), sso)
    rc, out, err = c.pb('team', 'scim', 'status', team)
    c.step('pb team scim status reads the directory state', rc == 0 and 'Directory:' in out, out + err)


def machine_access(c, start):
    """F19-07 in the personal workspace: an exec from this machine to --peer is recorded as
    machine.access_started; the peer, signed in with an API token that may only exec on itself,
    is refused exec on this machine and that is recorded as machine.access_denied with reason
    outside_token_scope and the token. Both are read back from the personal audit log. The peer's
    CLI session is replaced by the token and ends when it is revoked."""
    d = c.pbj('machine', 'list')
    machines = d if isinstance(d, list) else (d.get('machines') or d.get('items') or []) if isinstance(d, dict) else []
    ids = {m.get('alias'): m['id'] for m in machines}
    me, peer = ids.get(c.a.alias), ids.get(c.a.peer_alias)
    if not c.step('find this machine and the peer', me and peer, sorted(k for k in ids if k)):
        return
    marker = 'f19-access-%d' % time.time()
    for attempt in range(9):  # a new machine reaches the peer network asynchronously
        rc, out, err = c.pb('exec', peer, '--', 'echo', marker)
        if rc == 0 and marker in out:
            break
        time.sleep(10)
    c.step('exec on the peer works', rc == 0 and marker in out, out + err)
    tag = 'f19-%d' % time.time()
    rc, out, err = c.run(['python3', '.f19helper.py', 'token', c.a.pb, tag + '-read', 'personal', '.f19ptok'])
    rc2, out2, err2 = c.run(['python3', '.f19helper.py', 'token', c.a.pb, tag + '-scoped', 'personal', '.f19stok',
                             json.dumps([{'dst': ['machine:' + c.a.peer_alias], 'allow': ['exec']}])])
    if not c.step('create a reader token and a token that may only exec on the peer', rc == 0 and rc2 == 0, err + err2):
        return
    try:
        rc, secret, err = c.run(['cat', '.f19stok'])
        done = subprocess.run(shlex.split(c.a.peer_cmd) + ['bash -lc ' + shlex.quote(c.a.pb + ' login --api-token-file -')],
                              input=secret.strip() + '\n', capture_output=True, text=True, timeout=120)
        secret = ''
        c.step('the peer signs in with the scoped token', done.returncode == 0, done.stdout + done.stderr)
        done = subprocess.run(shlex.split(c.a.peer_cmd) + ['bash -lc ' + shlex.quote(c.a.pb + ' exec ' + me + ' -- true')],
                              capture_output=True, text=True, timeout=180)
        c.step('the peer is refused exec here (outside the token scope)', done.returncode != 0 and 'scope' in (done.stdout + done.stderr).lower(),
               done.stdout + done.stderr)

        def events():
            r = c.run(['env', 'F19TOK=.f19ptok', 'python3', '.f19helper.py', 'api', 'GET',
                       '/v1/activity?workspace=personal&action=machine&since=' + start + '&limit=100'])[1]
            try:
                data = json.loads(r).get('data') or {}
            except ValueError:
                return []
            return data.get('items') or [] if isinstance(data, dict) else []
        found = wait(lambda: (lambda e: e if any(x['action'] == 'machine.access_denied' for x in e) else None)(events()), 60) or events()
        started = [e for e in found if e['action'] == 'machine.access_started' and e['target'].get('id') == peer]
        denied = [e for e in found if e['action'] == 'machine.access_denied' and e['target'].get('id') == me]
        c.step('the access start is in the personal log (machine target, capability, actor)', started and started[0]['result'] == 'success'
               and started[0]['target']['kind'] == 'machine' and started[0]['details'].get('capability') == 'exec' and '@' in started[0]['actor'].get('email', ''), found)
        c.step('the refusal is in the personal log with reason and token', denied and denied[0]['result'] == 'denied'
               and denied[0].get('reason') == 'outside_token_scope' and denied[0]['actor'].get('token_id'), found)
    finally:
        for name in (tag + '-scoped', tag + '-read'):
            c.pb('api-token', 'revoke', name, '--yes', '--workspace', 'personal')
        c.run(['rm', '-f', '.f19ptok', '.f19stok'])


def search(c, start, owner):
    """F19-07/08: every filter narrows the log; events carry the documented shape; paging moves on."""
    items, raw = c.activity('--action', 'team.role', '--since', start)
    c.step('filter by action finds the 3 role changes', len(items) >= 3 and all(e['action'] == 'team.role' for e in items), raw)
    shape = items[0] if items else {}
    email = shape.get('actor', {}).get('email', '')
    c.step('an event has actor email, IP, user agent, request ID and result',
           shape.get('actor', {}).get('id') == owner and '@' in email and shape.get('ip') and shape.get('user_agent', '').startswith('paperboat')
           and shape.get('request_id') and shape.get('result') == 'success' and shape.get('target', {}).get('kind') == 'team', shape)
    by_actor, raw = c.activity('--actor', email, '--since', start)
    c.step('filter by actor email', by_actor and all(e['actor'].get('email') == email for e in by_actor), raw)
    found, raw = c.activity('--search', 'team.role', '--since', start)
    c.step('free-text search', len(found) >= 3, raw)
    ok, raw = c.activity('--result', 'success', '--since', start)
    denied, raw2 = c.activity('--result', 'denied', '--since', start)
    c.step('filter by result', ok and all(e['result'] == 'success' for e in ok) and all(e['result'] == 'denied' and e.get('reason') for e in denied), raw)
    future, raw = c.activity('--since', '2099-01-01T00:00:00Z')
    c.step('a time range with no events is empty', future == [] and isinstance(raw, dict) and 'items' in raw, raw)
    first = c.pbj('team', 'activity', c.a.team, '--limit', '1', '--since', start)
    nxt = c.pbj('team', 'activity', c.a.team, '--limit', '1', '--since', start, '--cursor', first.get('next_cursor', ''))
    c.step('cursor paging returns the next older event', first.get('items') and nxt.get('items') and
           nxt['items'][0]['id'] != first['items'][0]['id'] and nxt['items'][0]['occurred_at'] <= first['items'][0]['occurred_at'], [first, nxt])


def export(c, start):
    """F19-09: CSV and JSON exports carry the documented columns and the filtered events; each export is recorded."""
    rc, out, err = c.pb('team', 'activity', 'export', c.a.team, '--format', 'csv', '--action', 'team.role', '--since', start, '-o', 'f19.csv')
    rc2, body, _ = c.run(['cat', 'f19.csv'])
    rows = list(csv.reader(io.StringIO(body)))
    c.step('CSV export has the documented columns and the role changes', rc == 0 and rows and rows[0] == CSV_COLUMNS
           and len(rows) >= 4 and all(r[3] == 'team.role' for r in rows[1:]), (out + err + body)[:400])
    rc, out, err = c.pb('team', 'activity', 'export', c.a.team, '--format', 'json', '--since', start, '-o', 'f19.json')
    rc2, body, _ = c.run(['cat', 'f19.json'])
    try:
        events = json.loads(body)
    except ValueError:
        events = None
    c.step('JSON export is one array of events', rc == 0 and isinstance(events, list) and any(e['action'] == 'team.role' for e in events), out + err)
    c.run(['rm', '-f', 'f19.csv', 'f19.json'])
    items, raw = c.activity('--action', 'team.activity_exported', '--since', start)
    c.step('both exports are recorded', len(items) >= 2, raw)


def received(c):
    rc, out, err = c.run(['cat', 'f19recv.jsonl'])
    return [json.loads(l) for l in out.splitlines() if l.strip()]


def wait(cond, seconds=90):
    end = time.time() + seconds
    while True:
        got = cond()
        if got or time.time() > end:
            return got
        time.sleep(5)


def touch_role(c, member):
    """Makes two audit events (member → auditor → back) and returns their actions' count."""
    t = c.pbj('team', 'get', c.a.team)
    original = [m['role'] for m in t['members'] if m['account_id'] == member][0]
    for role in ['auditor', original]:
        gen = str(c.pbj('team', 'get', c.a.team)['generation'])
        c.pb('team', 'role', c.a.team, member, role, '--generation', gen)


def streams(c, team):
    """The team's stream destinations; [] when a poll fails (e.g. during a server rollout)."""
    data = c.api('GET', '/v1/teams/%s/audit-streams' % team).get('data')
    return [d for d in data if isinstance(d, dict)] if isinstance(data, list) else []


def tunnel(c, target):
    d = c.pbj('tunnel', '--ephemeral', target, '--background')
    return d.get('endpoint', ''), d.get('id', '')


def stream_https(c, team, member):
    """F19-10 HTTPS: signed NDJSON with the custom header, test event, in order, refused batch retried."""
    c.run(['rm', '-f', 'f19recv.jsonl', 'f19recv.fail'])
    c.api('DELETE', '/v1/teams/%s/audit-streams/https' % team)  # a stream left by an interrupted run
    c.run(['bash', '-c', 'nohup python3 .f19helper.py serve 8099 >/dev/null 2>&1 & for i in $(seq 20); do curl -s -o /dev/null http://127.0.0.1:8099/ && break; sleep 0.5; done'])
    url, tid = tunnel(c, '8099')
    if not c.step('publish the receiver with pb tunnel', url.startswith('https://'), url):
        return tid
    r = c.api('PUT', '/v1/teams/%s/audit-streams/https' % team, {'url': url + '/hook', 'header_name': 'X-F19-Check', 'header_value': '@.f19hdr'})
    c.step('configure the HTTPS stream (signing secret returned once)', r['status'] == 200 and r['data'].get('signing_secret') == '<stored>', r)
    r = c.api('POST', '/v1/teams/%s/audit-streams/https/test' % team)
    got = wait(lambda: [x for x in received(c) if any(e[1] == 'team.activity_stream_test' for e in x['events'])], 30)
    c.step('send a test event: signed, custom header, NDJSON', r['status'] in (200, 204) and got and got[0]['sig_ok'] and got[0]['header_ok']
           and got[0]['type'] == 'application/x-ndjson', [r, got])
    touch_role(c, member)
    got = wait(lambda: [e for x in received(c) for e in x['events'] if e[1] == 'team.role'][-2:] if len([e for x in received(c) for e in x['events'] if e[1] == 'team.role']) >= 2 else None)
    batches = received(c)
    c.step('new events arrive signed, within 90 s', got and all(x['sig_ok'] and x['header_ok'] for x in batches), batches[-3:])
    ids = [e[0] for x in batches for e in x['events'] if not e[0].startswith('test_')]
    c.step('events arrive in order without gaps from the stream start', ids == sorted(ids, key=ids.index) and len(ids) == len(set(ids)), ids)
    c.run(['touch', 'f19recv.fail'])
    touch_role(c, member)
    failing = wait(lambda: [d for d in streams(c, team) if d['kind'] == 'https' and d.get('last_error')], 90)
    c.step('a refused batch shows the error and pending events', failing and failing[0]['pending'] >= 1 and '503' in failing[0]['last_error'], failing)
    c.run(['rm', '-f', 'f19recv.fail'])
    before = len(ids)
    back = time.strftime('%Y-%m-%dT%H:%M:%S', time.gmtime())
    # Recovered: the error is cleared, a delivery happened after the receiver came back and the
    # held events arrived. (pending need not reach 0: events inside the 30 s settle window of a
    # busy team are always pending.)
    ok = wait(lambda: [d for d in streams(c, team) if d['kind'] == 'https' and not d.get('last_error')
                       and (d.get('last_delivered_at') or '') >= back], 600)  # backoff doubles from 10 s
    after = [e[0] for x in received(c) for e in x['events'] if not e[0].startswith('test_')]
    c.step('it is retried and delivered once the receiver is back', ok and len(after) >= before + 2, [ok, after[-4:]])
    r = c.api('DELETE', '/v1/teams/%s/audit-streams/https' % team)
    c.step('stop the HTTPS stream', r['status'] in (200, 204), r)
    return tid


def stream_s3(c, team, member):
    """F19-10 S3: gzip NDJSON objects under prefix/workspace/date, signed with SigV4, checked by a real S3 server."""
    c.run(['bash', '-c', 'nohup python3 .f19helper.py forward 8097 %s >/dev/null 2>&1 &' % shlex.quote(c.a.s3_origin)])
    time.sleep(1)
    c.api('DELETE', '/v1/teams/%s/audit-streams/s3' % team)
    url, tid = tunnel(c, '8097')
    if not c.step('publish the S3 store with pb tunnel', url.startswith('https://'), url):
        return tid
    r = c.api('PUT', '/v1/teams/%s/audit-streams/s3' % team, {'url': url, 'region': c.a.s3_region, 'bucket': c.a.s3_bucket,
              'prefix': 'f19', 'access_key_id': '@.f19-s3ak', 'secret_access_key': '@.f19-s3sk'})
    c.step('configure the S3 stream', r['status'] == 200, r)
    r = c.api('POST', '/v1/teams/%s/audit-streams/s3/test' % team)
    c.step('the S3 test upload is accepted (SigV4 verified by the store)', r['status'] in (200, 204), r)
    touch_role(c, member)

    def objects():
        done = subprocess.run(shlex.split(c.a.s3_list_cmd) + ['sh -c ' + shlex.quote('cd %s/%s && find f19 -name "*.ndjson.gz" -exec sh -c \'echo "== $1"; gunzip -c "$1"\' _ {} \\;' % (c.a.s3_root, c.a.s3_bucket))],
                              capture_output=True, text=True, timeout=60)
        return done.stdout if 'team.role' in done.stdout else None
    out = wait(objects, 120)
    key = (out or '').split('\n', 1)[0]
    c.step('events land as gzip NDJSON at f19/team-<team>/YYYY/MM/DD/<first>-<last>.ndjson.gz',
           out and key.startswith('== f19/team-%s/' % team) and key.endswith('.ndjson.gz'), (out or '')[:300])
    r = c.api('DELETE', '/v1/teams/%s/audit-streams/s3' % team)
    c.step('stop the S3 stream', r['status'] in (200, 204), r)
    return tid


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument('--team', required=True)
    p.add_argument('--cmd', required=True)
    p.add_argument('--pb', default='/home/tester/.local/bin/pb')
    p.add_argument('--s3-root', default='/tmp/vgw', help='the store\'s data directory in --s3-list-cmd')
    p.add_argument('--api', default='https://api.pprbt.dev')
    p.add_argument('--alias', default='', help='this machine\'s name, for the F19-07 access steps')
    p.add_argument('--peer-alias', default='', help='a second throwaway machine of the account (F19-07)')
    p.add_argument('--peer-cmd', default='', help='runs commands on the peer; its CLI session is replaced')
    p.add_argument('--s3-origin', default='', help='host:port of the S3-compatible store, reachable from the machine')
    p.add_argument('--s3-list-cmd', default='')
    p.add_argument('--s3-region', default='eu-central-1')
    p.add_argument('--s3-bucket', default='audit')
    a = p.parse_args()
    c = Check(a)
    c.run(['bash', '-c', 'umask 077; cat > .f19helper.py; printf %s ' + shlex.quote(a.api) + ' > .f19api'], stdin=HELPER)
    owner = c.pbj('team', 'get', a.team).get('owner_account')
    start = time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime(time.time() - 5))
    member = roles(c, a.team)
    identity(c, a.team)
    if a.alias and a.peer_cmd:
        machine_access(c, start)
    token = 'f19-check-%d' % time.time()
    rc, out, err = c.run(['python3', '.f19helper.py', 'token', a.pb, token, a.team])
    c.step('create a one-day API token for stream setup', rc == 0 and out.strip() != '', err[-300:])
    search(c, start, owner)
    export(c, start)
    tunnels = []
    try:
        if member:
            tunnels.append(stream_https(c, a.team, member))
            if a.s3_origin:
                tunnels.append(stream_s3(c, a.team, member))
    finally:
        for kind in ('https', 's3'):
            c.api('DELETE', '/v1/teams/%s/audit-streams/%s' % (a.team, kind))
        for tid in tunnels:
            if tid:
                c.pb('tunnel', 'stop', tid)
        c.run(['bash', '-c', 'for f in .f19serve.pid .f19forward.pid; do [ -f $f ] && kill $(cat $f); done; rm -f f19recv.jsonl f19recv.fail .f19sig .f19hdr .f19*.pid'])
        c.pb('api-token', 'revoke', token, '--yes', '--workspace', a.team)
        c.run(['rm', '-f', '.f19tok', '.f19helper.py', '.f19api'])
    print('RESULT', 'PASS' if not c.fails else 'FAIL ' + ','.join(c.fails))
    return 1 if c.fails else 0


if __name__ == '__main__':
    sys.exit(main())
