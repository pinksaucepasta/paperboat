#!/usr/bin/env python3
"""F21-37 on STAGING ONLY: the edge's idle limit on public TCP, TLS passthrough and private `pb access` streams.

Usage: fixture_checks.py staging-edge-idle   (smoke.sh row of the same name)
The edges must already run with a 120 s limit: every ready staging edge, because every route is assigned to each and
`pb access` takes whichever the server lists first. Set it, and restore it afterwards, with deploy/staging.py
(`edge` and `edge-b`, STAGING_PUBLIC_TCP_IDLE_SECONDS=120 and then without it); the row never restarts an edge.
With the routes' own idle limit at 90 s, one quiet and one busy (a byte every 10 s) stream are held on each path: the
quiet ones must close 115-130 s after their last byte with a clean EOF (no reset), the busy ones must be alive at 200 s,
and an HTTPS tunnel on the same edge must answer 200 every 30 s.
Settings: those of staging_edge_outage.py, with every ready edge in SMOKE_STAGING_EDGES (the first one also publishes
the public TCP ports), plus SMOKE_STAGING_CLIENT (the second paired staging fixture, default pb-stg-b). The public
streams run from this host straight to the edge's ports: a forward would turn a reset into an EOF.
Safety: it refuses to run from a checkout behind origin/main in tools/device-smoke or deploy/staging.py, and before it
changes anything it proves every target is staging (staging_edge_outage.Staging.guard). One run at a time (the staging
host's edge lock directory). Its tunnels, `pb access` and origin also expire by themselves.
"""
import concurrent.futures
import copy
import datetime
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import subprocess
import sys
import time
import urllib.parse

import idle_streams
from ct_env import CT_DOCKER as DOCKER, CT_USER
from staging_edge_outage import CONTAINER_USER, DATABASE, LOCK, Refused, Run, Staging, delete_tunnel, settings

LIMIT = 120  # the edge's public_tcp_idle_timeout for this row, s
ROUTE_IDLE = 90  # the routes' own idle limit: a quiet stream must outlive it
CLOSE_MIN, CLOSE_MAX = 115, 130
HOLD = 200  # a busy stream must be alive this long
EVERY = 10  # a busy stream's byte
OPEN_SECONDS = 120  # for a new route to serve
HTTPS_EVERY = 30
GUARDED = ('tools/device-smoke', 'deploy/staging.py')
PROBE = Path(__file__).resolve().parent / 'idle_streams.py'


def outdated(source, run=subprocess.run):
    """None, or why the checkout that holds `source` may not run the row: origin/main has commits in the harness or in
    deploy/staging.py that it lacks (an old row or an old staging.py would disturb shared staging in an old way)."""
    def git(*words):
        return run(['git', '-C', str(source), *words], capture_output=True, text=True, timeout=120, stdin=subprocess.DEVNULL)
    top = git('rev-parse', '--show-toplevel')
    if top.returncode:
        return '%s is not in a git checkout, so its age cannot be checked' % source
    source = top.stdout.strip()
    if git('fetch', '-q', 'origin', 'main').returncode:
        return 'origin/main cannot be fetched, so the checkout\'s age cannot be checked'
    behind = git('log', '--format=%h %s', 'HEAD..origin/main', '--', *GUARDED)
    if behind.returncode:
        return 'the checkout cannot be compared with origin/main'
    lines = behind.stdout.splitlines()
    if lines:
        return 'outdated checkout: %d commit(s) on origin/main touch %s and are not in HEAD, newest: %s. Update the checkout first.' % (
            len(lines), ' or '.join(GUARDED), lines[0][:100])


def source_directory():
    """smoke.sh runs a frozen copy of the harness; the checkout it was taken from is the one that has an age."""
    return os.environ.get('PAPERBOAT_SMOKE_SOURCE_DIR') or str(PROBE.parent)


def ready_edges(s):
    query = ("select id from paperboat.control_tunnel_nodes where 'edge'=any(roles) and state='ready' and ready "
             "and last_heartbeat_at>now()-interval '2 minutes' order by id")
    return s.ssh('%s exec %s psql -U postgres -d paperboat_staging -Atc %s' % (DOCKER, DATABASE, shlex.quote(query))).stdout.split()


def idle_limit(s, container):
    """The edge's configured public_tcp_idle_timeout in seconds (0: the default)."""
    out = s.ssh('f=$(%s inspect -f %s %s) && sudo -n cat "$f/deployment.json" | python3 -c %s' % (
        DOCKER, shlex.quote('{{range .Mounts}}{{if eq .Destination "/state"}}{{.Source}}{{end}}{{end}}'), container,
        shlex.quote('import json,sys; print(json.load(sys.stdin).get("public_tcp_idle_timeout", 0))'))).stdout.strip()
    return int(out) / 1e9


def guard(s, client):
    """Read only. Everything the outage row proves, the second fixture too, and that no ready edge keeps another limit."""
    s.guard()
    if s.edges[0][0] in s.dials:
        raise Refused('the first listed edge %s is reached through a forward, which would turn a reset into an EOF' % s.edges[0][0])
    if not re.fullmatch(r'pb-stg-[a-z0-9-]+', client.machine) or client.machine == s.machine:
        raise Refused('the staging client machine %r is not a second pb-stg- container' % client.machine)
    code, shown = client.pb('config', 'show', check=False)
    server = (shown.get('data') or shown).get('server_url', '').rstrip('/')
    if code or server != s.api:
        raise Refused('the fixture %s is signed in to %r, not to the staging API' % (client.machine, server))
    ready, listed = ready_edges(s), {node: container for container, _, node in s.edges}
    unlisted = sorted(set(ready) - set(listed))
    if unlisted:
        raise Refused('ready staging edge(s) %s are not in SMOKE_STAGING_EDGES: a private stream could go through one' % ', '.join(unlisted))
    if s.edges[0][2] not in ready:
        raise Refused('the first listed edge %s is not ready' % s.edges[0][0])
    for container, _, node in s.edges:
        if node not in ready:
            continue
        limit = idle_limit(s, container)
        if limit != LIMIT:
            raise Refused('%s runs with idle limit %s, not %d s. Restart every ready edge with STAGING_PUBLIC_TCP_IDLE_SECONDS=%d '
                          '(deploy/staging.py edge IMAGE, edge-b IMAGE), and without it afterwards' % (
                              container, '%g s' % limit if limit else 'at the default', LIMIT, LIMIT))
    return [listed[node] for node in ready]


def stamp_at(run, when, what):
    run.samples.append('\t'.join((when, what, '', '', '')))


def judge(result):
    """(ok, detail) of one held stream against the row's rule for it."""
    if not result.get('opened'):
        return False, result.get('error', 'no result')
    times = 'opened %s' % clock_of(result['opened_utc'])
    if result['closed']:
        what = '%s %s s after the last byte at %s, %s' % (
            result['closed'].upper() if result['closed'] in ('eof', 'rst') else result['closed'], result['after'], clock_of(result['closed_utc']), times)
    else:
        what = 'alive after %s s at %s, %s' % (result['age'], clock_of(result['alive_utc']), times)
    if result['busy']:
        what += ', %d of %d bytes echoed' % (result['echoed'], result['sent'])
        return result['closed'] is None and result['age'] >= HOLD and result['sent'] >= HOLD // EVERY and result['echoed'] == result['sent'], what
    return result['closed'] == 'eof' and CLOSE_MIN <= result['after'] <= CLOSE_MAX, what


def clock_of(stamp):
    return datetime.datetime.fromisoformat(stamp).strftime('%H:%M:%S.%f')[:-5] + 'Z'


def copy_in(s, path, text):
    s.ssh('%s exec -i %s %s sh -c %s' % (DOCKER, CONTAINER_USER, s.machine, shlex.quote('cat > %s' % shlex.quote(path))), stdin=text)


def parsed_streams(done):
    """The holder's result line out of an ssh run."""
    try:
        return json.loads(done.stdout.strip().splitlines()[-1])['streams']
    except (ValueError, IndexError, KeyError, TypeError):
        raise RuntimeError('the stream holder gave no result: exit %d %s' % (done.returncode, (done.stdout + done.stderr)[-200:]))


class Holders:
    """The two places streams are held, each running idle_streams.py: this host (public paths) and the client fixture."""

    def __init__(self, client):
        self.client = client

    def public(self, plan):
        return idle_streams.hold_all(plan)

    def private(self, plan):
        command = 'python3 - hold ' + shlex.quote(json.dumps(plan))
        return parsed_streams(self.client.ssh('%s exec -i %s %s sh -c %s' % (DOCKER, CONTAINER_USER, self.client.machine, shlex.quote(command)),
                                              timeout=plan['hold'] + plan['open'] + 90, check=False, stdin=PROBE.read_text()))


def plan(streams):
    return {'streams': streams, 'hold': HOLD + 5, 'every': EVERY, 'open': OPEN_SECONDS}


def sample_https(run, url, seen):
    began = run.clock()
    status, ok = run.served(url)
    seen.append((began, status, ok))


def hold_streams(run, holders, url, public, private, submit):
    """Both holders at once, with the HTTPS URL sampled every HTTPS_EVERY s until they are done, and once after."""
    seen = []
    jobs = [submit(holders.public, plan(public)), submit(holders.private, plan(private))]
    due = run.clock()
    while not all(job.done() for job in jobs):
        if run.clock() >= due:
            sample_https(run, url, seen)
            due += HTTPS_EVERY
        run.sleep(1)
    sample_https(run, url, seen)
    results = []
    for job, specs in zip(jobs, (public, private)):
        try:
            results += job.result()
        except Exception as error:  # noqa: BLE001 a holder that failed fails its own streams, never the others'
            results += [{'name': spec['name'], 'busy': spec['busy'], 'opened': False, 'error': 'the holder failed: %s' % str(error)[:200]} for spec in specs]
    return results, seen


def route_of(s, tunnel, protocol):
    _, listed = s.pb('tunnel', 'route', 'list', tunnel)
    for route in listed.get('items', []):
        if route.get('protocol') == protocol:
            return route
    raise RuntimeError('tunnel %s has no %s route' % (tunnel, protocol))


def drill(run, client, holders, tag, leftovers, submit):
    """Create, hold, check, delete. Every exit path removes what it made."""
    s = run.s
    edges = guard(s, client)  # before anything changes
    run.log(True, 'every target is staging and every ready edge has a %d s idle limit' % LIMIT, ', '.join(edges))
    cleanups = []
    got = s.ssh('mkdir %s && { echo %s > %s/owner || { rmdir %s; false; }; }' % (LOCK, tag, LOCK, LOCK), check=False)
    if got.returncode:
        run.log(False, 'no other staging edge disturbance is running', 'lock %s on the staging host is held (remove it only when no run is active)' % LOCK)
        return
    try:
        edge, https_port, _ = s.edges[0]
        work = '/tmp/%s' % tag
        base = 8700 + 4 * secrets.randbelow(200)
        echo_port, tls_port, http_port, access_port = base, base + 1, base + 2, base + 3
        s.in_machine('mkdir -m 700 %s' % work)
        cleanups.append(('origin', lambda: s.in_machine("pkill -f '%s/[i]dle_streams.py origin'; rm -rf %s" % (work, work))))
        copy_in(s, work + '/idle_streams.py', PROBE.read_text())
        pin = s.in_machine('cd %s && openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 1 -subj /CN=%s-origin '
                           '-keyout tls.key -out tls.crt 2>/dev/null && openssl x509 -in tls.crt -outform der | sha256sum' % (work, tag)).stdout.split()[0]
        s.ssh('%s exec -d -u %s %s python3 %s/idle_streams.py origin %s %d %d %d %s' % (
            DOCKER, CT_USER, s.machine, work, work, echo_port, tls_port, http_port, run.marker))
        s.in_machine('python3 -c %s %d %d %d' % (shlex.quote(
            'import socket,sys,time\nfor p in map(int,sys.argv[1:]):\n for n in range(50):\n  try: socket.create_connection(("127.0.0.1",p),1).close(); break\n'
            '  except OSError: time.sleep(0.2)\n else: sys.exit("origin port %d is not listening" % p)'), echo_port, tls_port, http_port))

        names = {'web': '%s-web' % tag, 'tcp': '%s-tcp' % tag, 'priv': '%s-priv' % tag}
        made = {}
        for kind, extra in (('web', ['--port', str(http_port)]), ('tcp', ['--from', 'tcp://127.0.0.1:%d' % echo_port]),
                            ('priv', ['--from', 'tcp://127.0.0.1:%d' % echo_port, '--private'])):
            # registered before the create: a create that failed after the server accepted it is found by its name
            cleanups.append(('tunnel %s' % names[kind], lambda name=names[kind]: delete_tunnel(s, name, missing_ok=True)))
            _, made[kind] = s.pb('tunnel', 'create', names[kind], *extra, '--duration', '1h', '--wait')
        s.pb('tunnel', 'route', 'add', names['tcp'], '--name', 'pass', '--protocol', 'tls', '--to', 'tcp://127.0.0.1:%d' % tls_port,
             '--idle-timeout', '%ds' % ROUTE_IDLE, '--wait')
        routes = {'public TCP': route_of(s, names['tcp'], 'tcp'), 'TLS passthrough': route_of(s, names['tcp'], 'tls'),
                  'private TCP via pb access': route_of(s, names['priv'], 'tcp_private')}
        idles = {path: route.get('idle_timeout_ms') for path, route in routes.items()}
        run.log(set(idles.values()) == {ROUTE_IDLE * 1000}, "every route's own idle limit is %d s" % ROUTE_IDLE, ', '.join('%s %s ms' % i for i in idles.items()))
        tcp_port, tcp_host = routes['public TCP']['public_tcp_port'], routes['public TCP']['host_match']['hostname']
        tls_host = routes['TLS passthrough']['host_match']['hostname']
        for name in (tcp_host, tls_host):
            if 'stg' not in name.split('.'):
                raise RuntimeError('the route name %r is not a staging name' % name)
        bound = s.ssh('%s port %s %d/tcp' % (DOCKER, edge, tcp_port), check=False)
        if not run.log(bound.returncode == 0 and bool(bound.stdout.strip()), '%s publishes the public TCP port %d' % (edge, tcp_port), ' '.join(bound.stdout.split())):
            return

        web = made['web']['tunnel']['stable_endpoint']
        web_host = urllib.parse.urlsplit(web).hostname or ''
        if 'stg' not in web_host.split('.'):
            raise RuntimeError('the endpoint %r is not a staging name' % web)
        url = 'https://%s:%d/' % (web_host, https_port)
        run.names[url] = 'HTTPS on %s' % edge
        up = run.until_served({'https': url}, 180)['https']
        if not run.log(up[1], 'HTTPS on %s serves before the streams open' % edge, 'HTTP %d' % up[0]):
            return

        pattern = '[p]b access tunnel %s ' % names['priv']
        cleanups.append(('pb access on %s' % client.machine, lambda: client.in_machine(
            "pkill -INT -f '%s'; for n in 1 2 3 4 5 6 7 8 9 10; do pgrep -f '%s' >/dev/null || break; sleep 1; done; pkill -KILL -f '%s'; rm -rf %s" % (
                pattern, pattern, pattern, work))))
        client.in_machine('mkdir -m 700 %s' % work)
        command = 'exec timeout -s INT 600 .local/bin/pb access tunnel %s --listen 127.0.0.1:%d --json > %s/access.log 2>&1' % (names['priv'], access_port, work)
        # the route appears on the client after its next route list: pb access is retried until it listens
        client.ssh('%s exec -d %s %s sh -c %s' % (DOCKER, CONTAINER_USER, client.machine, shlex.quote(command)))
        said, deadline = '', run.clock() + 90
        while run.clock() < deadline:
            said = client.in_machine('cat %s/access.log' % work, check=False).stdout
            if '"listen_address"' in said:
                break
            if '"ok":false' in said:
                run.sleep(5)
                client.ssh('%s exec -d %s %s sh -c %s' % (DOCKER, CONTAINER_USER, client.machine, shlex.quote(command)))
            run.sleep(2)
        if not run.log('"listen_address"' in said, 'pb access on %s listens' % client.machine, ' '.join(said.split())[-200:] if '"listen_address"' not in said else '127.0.0.1:%d' % access_port):
            private = []
        else:
            private = [{'name': 'private TCP via pb access %s' % kind, 'host': '127.0.0.1', 'port': access_port, 'busy': kind == 'busy'} for kind in ('quiet', 'busy')]
        public = [{'name': 'public TCP %s' % kind, 'host': tcp_host, 'port': tcp_port, 'busy': kind == 'busy'} for kind in ('quiet', 'busy')]
        public += [{'name': 'TLS passthrough %s' % kind, 'host': tls_host, 'port': https_port, 'sni': tls_host, 'pin': pin, 'busy': kind == 'busy'}
                   for kind in ('quiet', 'busy')]
        run.stamp('holding: %d public and %d private streams' % (len(public), len(private)))
        results, seen = hold_streams(run, holders, url, public, private, submit)
        for result in results:
            if result.get('opened'):
                stamp_at(run, result['opened_utc'], 'stream opened: %s' % result['name'])
                stamp_at(run, result.get('closed_utc') or result['alive_utc'],
                         'stream %s: %s' % ('closed (%s)' % result['closed'] if result['closed'] else 'still alive', result['name']))
            ok, detail = judge(result)
            rule = 'is alive at %d s' % HOLD if result['busy'] else 'closes with EOF %d-%d s after its last byte' % (CLOSE_MIN, CLOSE_MAX)
            run.log(ok, '%s stream %s' % (result['name'], rule), detail)
        missed = [(round(at - seen[0][0]), status) for at, status, ok in seen if not ok]
        run.log(len(seen) >= HOLD // HTTPS_EVERY + 1 and not missed, 'HTTPS on %s answers 200 every %d s while the streams are held' % (edge, HTTPS_EVERY),
                '%d of %d answered 200 with the marker over %d s%s' % (len(seen) - len(missed), len(seen), round(seen[-1][0] - seen[0][0]),
                                                                      '; misses (s, status) %s' % missed[:4] if missed else ''))
    finally:
        failed = len(leftovers)
        for what, cleanup in reversed(cleanups):
            try:
                cleanup()
            except Exception as error:  # noqa: BLE001
                leftovers.append('staging %s NOT removed: %s' % (what, str(error)[:150]))
        try:
            s.ssh('rm -f %s/owner && rmdir %s' % (LOCK, LOCK))
        except Exception as error:  # noqa: BLE001
            leftovers.append('staging lock %s NOT removed (remove it when no run is active): %s' % (LOCK, str(error)[:120]))
        run.stamp('cleanup done, %d leftover(s)' % (len(leftovers) - failed))


def keep_timeline(run, tag):
    """The timeline in time order, mode 600 from the start. A file that cannot be written is said so, never the result."""
    try:
        from results_dir import results_dir
        folder = results_dir(__file__) / 'fixture-checks'
        folder.mkdir(parents=True, exist_ok=True)
        timeline = folder / ('staging-edge-idle-%s-%s.tsv' % (time.strftime('%Y%m%dT%H%M%SZ', time.gmtime()), tag))
        with open(os.open(timeline, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), 'w') as handle:
            handle.write('\n'.join(sorted(run.samples, key=lambda line: line.split('\t')[0])) + '\n')
        print('timeline: %s' % timeline, file=sys.stderr, flush=True)
    except OSError as error:
        print('timeline NOT kept: %s' % str(error)[:200], file=sys.stderr, flush=True)


def check(m):
    """The fixture_checks.py row: (ok, detail). m.leftovers receives anything cleanup could not remove."""
    import console_rows
    env, host = settings()
    tag = 'edgeidle-' + secrets.token_hex(4)
    staging = Staging(env, host)
    client = copy.copy(staging)
    client.machine = env.get('SMOKE_STAGING_CLIENT', 'pb-stg-b')
    run = Run(staging, 'pb-%s-marker' % tag)
    try:
        reason = outdated(source_directory())
        if reason:
            raise Refused(reason)
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            drill(run, client, Holders(client), tag, m.leftovers, pool.submit)
    except Exception as error:  # noqa: BLE001 a FAIL with the reason, after the drill's own cleanup
        run.log(False, 'the drill stopped', '%s: %s' % (type(error).__name__, str(error)[:300]))
    keep_timeline(run, tag)
    return console_rows.verdict('staging-edge-idle', [('staging-edge-idle', 1 if run.failed else 0, '\n'.join(run.lines))])
