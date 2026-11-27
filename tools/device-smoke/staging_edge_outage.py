#!/usr/bin/env python3
"""F21-17 on STAGING ONLY: a public preview and a public tunnel keep serving on every staging edge while the edges are
cut off the control server for 100 s, stay served after it returns, and drop only when each is deleted.

Usage: fixture_checks.py staging-edge-outage   (smoke.sh row of the same name)
Settings: staging_host (targets.json top level), SMOKE_STAGING_API (the staging API URL), SMOKE_STAGING_MACHINE (a paired
staging fixture container there, default pb-stg-a), SMOKE_STAGING_EDGES (CONTAINER:PUBLIC_HTTPS_PORT:NODE_ID, comma
separated, default pb-stg-edge:18446:stg_managed_edge).
Safety: before anything changes, the API host must carry an `stg` label and not be the operator's production API, the
fixture's pb must be signed in to exactly that API, and every edge must be a pb-stg-edge container whose own
control_url is that API. The cut is DROP rules on the control port inside the edge's network namespace only (public
traffic still arrives), tagged with this run's id; a timer on the staging host removes them even if this process dies.
One run at a time (a lock directory on the staging host). The tunnel and preview also expire by themselves.
"""
import json
import re
import secrets
import shlex
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request

from ct_env import CT_DOCKER as DOCKER, CT_HOME, CT_RUNDIR, CT_USER

OUTAGE_SECONDS = 100  # the cell asks for 90 s or more
RECOVERY_SECONDS = 60  # an earlier recovery bug withdrew decisions about 20 s after control returned
DROP_SECONDS = 60
HELD_SECONDS = 15
DEAD_MAN_SECONDS = 420
STALE_AT_CUT_END = 60  # the server's age of the edge's last heartbeat: it really stopped hearing from the edge
LOCK = '/tmp/pb-staging-edge-disturb.lock'
SERVER = 'pb-release-staging-server-1'
DATABASE = 'pb-release-staging-postgres-1'
CONTAINER_USER = '-u %s -e HOME=%s -e XDG_RUNTIME_DIR=%s -w %s' % (CT_USER, CT_HOME, CT_RUNDIR, CT_HOME)


def fetch(url, timeout=8):
    """(status, body) of one new connection with the certificate verified; status 0 when there was no HTTP answer."""
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, *args):
            return None
    try:
        with urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect).open(url, timeout=timeout) as reply:
            return reply.status, reply.read(4096).decode(errors='replace')
    except urllib.error.HTTPError as error:
        return error.code, ''
    except (OSError, ValueError):
        return 0, ''


class Refused(RuntimeError):
    """A safety guard failed: nothing was changed."""


class Staging:
    def __init__(self, env, host, run=subprocess.run):
        self.host, self.run = host or '', run
        self.api = env.get('SMOKE_STAGING_API', '').rstrip('/')
        self.production = env.get('SMOKE_OPERATOR_API', '')
        self.machine = env.get('SMOKE_STAGING_MACHINE', 'pb-stg-a')
        self.edges = []
        for item in env.get('SMOKE_STAGING_EDGES', 'pb-stg-edge:18446:stg_managed_edge').split(','):
            container, port, node = item.strip().split(':')
            self.edges.append((container, int(port), node))

    def ssh(self, script, timeout=90, check=True, stdin=None):
        done = self.run(['ssh', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=15', self.host, script], capture_output=True, text=True,
                        timeout=timeout, **({'input': stdin} if stdin is not None else {'stdin': subprocess.DEVNULL}))
        if check and done.returncode:
            raise RuntimeError('staging host command failed (%d): %s' % (done.returncode, (done.stdout + done.stderr)[-200:]))
        return done

    def in_machine(self, command, **kw):
        return self.ssh('%s exec %s %s sh -c %s' % (DOCKER, CONTAINER_USER, self.machine, shlex.quote(command)), **kw)

    def pb(self, *words, check=True, timeout=200):
        done = self.in_machine('.local/bin/pb ' + ' '.join(map(shlex.quote, words)) + ' --json', check=False, timeout=timeout)
        try:
            value = json.loads(done.stdout.strip().splitlines()[-1])
        except (ValueError, IndexError):
            value = {}
        if check and (done.returncode or not value):
            raise RuntimeError('pb %s failed: exit %d: %s' % (' '.join(words[:2]), done.returncode, (done.stdout + done.stderr)[-200:]))
        return done.returncode, value

    def guard(self):
        """Read only. Raises Refused unless every target is provably staging."""
        parsed = urllib.parse.urlsplit(self.api)
        name = parsed.hostname or ''
        if parsed.scheme != 'https' or 'stg' not in name.split('.') or not parsed.port:
            raise Refused('SMOKE_STAGING_API is not a staging API URL (https://REGION.stg.DOMAIN:PORT): %r' % self.api)
        if self.production and urllib.parse.urlsplit(self.production).hostname == name:
            raise Refused('SMOKE_STAGING_API is the production API')
        if not self.host:
            raise Refused('staging_host is not set in the local targets')
        if not re.fullmatch(r'pb-stg-[a-z0-9-]+', self.machine):
            raise Refused('the staging fixture machine %r is not a pb-stg- container' % self.machine)
        for container, _, node in self.edges:
            if not re.fullmatch(r'pb-stg-edge[a-z0-9-]*', container) or not re.fullmatch(r'stg_[a-z0-9_]+', node):
                raise Refused('%s / %s is not a staging edge container and node' % (container, node))
            control = self.ssh('f=$(%s inspect -f %s %s) && sudo -n cat "$f/deployment.json" | python3 -c %s' % (
                DOCKER, shlex.quote('{{range .Mounts}}{{if eq .Destination "/state"}}{{.Source}}{{end}}{{end}}'), container,
                shlex.quote('import json,sys; print(json.load(sys.stdin)["control_url"])'))).stdout.strip().rstrip('/')
            if control != self.api:
                raise Refused('edge %s reports to %r, not to the staging API' % (container, control))
        code, shown = self.pb('config', 'show', check=False)
        server = (shown.get('data') or shown).get('server_url', '').rstrip('/')
        if code or server != self.api:
            raise Refused('the fixture %s is signed in to %r, not to the staging API' % (self.machine, server))

    def heartbeat_age(self, node):
        query = ("select state, ready, round(extract(epoch from now()-last_heartbeat_at)) from paperboat.control_tunnel_nodes "
                 "where id='%s'" % node)  # node matched stg_[a-z0-9_]+ in the guard
        out = self.ssh('%s exec %s psql -U postgres -d paperboat_staging -Atc %s' % (DOCKER, DATABASE, shlex.quote(query))).stdout.strip()
        state, ready, age = out.split('|')
        return state == 'ready' and ready == 't', int(float(age))

    def netns(self, container):
        pid = self.ssh("%s inspect -f '{{.State.Pid}}' %s" % (DOCKER, container)).stdout.strip()
        if not pid.isdigit() or int(pid) < 2:
            raise RuntimeError('edge %s is not running' % container)
        return 'nsenter -t %s -n' % pid

    def rules(self, netns):
        return self.ssh('sudo -n %s iptables -L OUTPUT -v -n -x' % netns).stdout

    def server_address(self):
        address = self.ssh("%s inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' %s" % (DOCKER, SERVER)).stdout.split()
        if not address or not re.fullmatch(r'[0-9.]+', address[0]):
            raise RuntimeError('the staging server container has no address')
        return address[0]


def cut_rules(netns, port, address, tag):
    """(insert, undo) shell for the root of the staging host: DROP everything this edge sends to the control server."""
    matches = ['-p tcp --dport %d' % port, '-d %s' % address]
    comment = '-m comment --comment %s' % tag
    insert = ' && '.join('%s iptables -I OUTPUT %s %s -j DROP' % (netns, match, comment) for match in matches)
    undo = '; '.join('while %s iptables -D OUTPUT %s %s -j DROP 2>/dev/null; do :; done' % (netns, match, comment) for match in matches)
    return insert, undo


def origin_cleanup(port, work):
    """Stops the run's origin and removes its directory. `[h]ttp` keeps pkill -f from matching the shell that runs this."""
    return "pkill -f '[h]ttp.server %d --bind 127.0.0.1 --directory %s'; rm -rf %s" % (port, work, work)


class Run:
    def __init__(self, staging, marker, sleep=time.sleep, clock=time.monotonic, fetch=fetch):
        self.s, self.marker, self.sleep, self.clock, self.fetch = staging, marker, sleep, clock, fetch
        self.lines, self.failed = [], 0

    def log(self, ok, step, detail=''):
        self.failed += not ok
        line = '%s  %s%s' % ('PASS' if ok else 'FAIL', step, '  (%s)' % detail if detail else '')
        self.lines.append(line)
        print(line, flush=True)
        return ok

    def addresses(self, endpoint):
        """The endpoint on every edge's public HTTPS port: [(edge, url)]. The advertised URL carries no port on staging."""
        parsed = urllib.parse.urlsplit(endpoint)
        if parsed.scheme != 'https' or 'stg' not in (parsed.hostname or '').split('.'):
            raise RuntimeError('the endpoint %r is not a staging name' % endpoint)
        return [(container, 'https://%s:%d/' % (parsed.hostname, port)) for container, port, _ in self.s.edges]

    def served(self, url):
        status, body = self.fetch(url)
        return status, status == 200 and self.marker in body

    def until_served(self, urls, seconds):
        deadline, last = self.clock() + seconds, {}
        while True:
            last = {key: self.served(url) for key, url in urls.items()}
            if all(ok for _, ok in last.values()) or self.clock() >= deadline:
                return last
            self.sleep(3)

    def watch(self, urls, seconds):
        """Samples every URL about every 2 s for `seconds`: {key: (samples, [(second, status) of each miss])}."""
        seen = {key: [0, []] for key in urls}
        began = self.clock()
        while True:
            for key, url in urls.items():
                status, ok = self.served(url)
                seen[key][0] += 1
                if not ok:
                    seen[key][1].append((round(self.clock() - began), status))
            if self.clock() - began >= seconds:
                return seen
            self.sleep(2)

    def all_served(self, seen, when):
        for key, (samples, missed) in seen.items():
            self.log(samples >= 5 and not missed, '%s keeps serving %s' % (key, when),
                     '%d of %d answered 200 with the marker%s' % (samples - len(missed), samples, '; first misses (s, status) %s' % missed[:4] if missed else ''))

    def dropped(self, urls, others, what):
        """After a delete: each of `urls` settles on the edge's own 404 and stays there; `others` keep serving."""
        began, pending, other_missed = self.clock(), dict(urls), []
        while pending and self.clock() - began < DROP_SECONDS:
            for key, url in list(pending.items()):
                if self.served(url)[0] == 404:
                    del pending[key]
            if pending:
                self.sleep(2)
        took = round(self.clock() - began)
        self.log(not pending, 'deleting the %s drops it on every edge' % what, 'HTTP 404 after %d s' % took if not pending else 'still not 404 after %d s: %s' % (took, ', '.join(pending)))
        held = {key: [] for key in urls}
        for _ in range(HELD_SECONDS // 3):
            self.sleep(3)
            for key, url in urls.items():
                held[key].append(self.served(url)[0])
            other_missed += [key for key, url in others.items() if not self.served(url)[1]]
        self.log(not pending and all(set(statuses) == {404} for statuses in held.values()), 'the deleted %s stays dropped' % what, str({k: sorted(set(v)) for k, v in held.items()}))
        if others:
            self.log(not other_missed, 'deleting the %s leaves the rest serving' % what, ', '.join(sorted(set(other_missed))))


def drill(run, tag, leftovers):
    """Create, check, cut, check, restore, check, delete, check. Every exit path restores the edges and removes what it made."""
    s = run.s
    s.guard()  # before anything changes
    run.log(True, 'every target is staging: API, fixture sign-in and each edge\'s control_url')
    undo, cleanups, netns = [], [], {}
    got = s.ssh('mkdir %s && { echo %s > %s/owner || { rmdir %s; false; }; }' % (LOCK, tag, LOCK, LOCK), check=False)
    if got.returncode:
        run.log(False, 'no other staging edge disturbance is running', 'lock %s on the staging host is held (remove it only when no run is active)' % LOCK)
        return
    try:
        for container, _, node in s.edges:
            netns[container] = s.netns(container)
            if 'DROP' in s.rules(netns[container]):
                run.log(False, '%s starts without DROP rules' % container, 'rules left by someone else: not disturbing')
                return
            healthy, age = s.heartbeat_age(node)
            if not run.log(healthy and age < 30, '%s is ready and heartbeating before the cut' % container, 'heartbeat %d s old' % age):
                return
        work, port = '/tmp/%s' % tag, 8700 + secrets.randbelow(300)
        s.in_machine('mkdir -m 700 %s && echo %s > %s/index.html' % (work, run.marker, work))
        cleanups.append(('origin', lambda: s.in_machine(origin_cleanup(port, work))))
        s.ssh('%s exec -d -u %s %s python3 -m http.server %d --bind 127.0.0.1 --directory %s' % (DOCKER, CT_USER, s.machine, port, work))
        made = {}
        # registered before the create: a create that failed after the server accepted it is found by its name and removed
        gone = set()  # what the drill itself deleted: cleanup leaves it alone
        cleanups.append(('tunnel %s' % tag, lambda: 'tunnel' in gone or delete_tunnel(s, made.get('tunnel', (tag,))[0], missing_ok=True)))
        _, created = s.pb('tunnel', 'create', tag, '--port', str(port), '--duration', '1h', '--wait')
        made['tunnel'] = (created['tunnel']['id'], created['tunnel']['stable_endpoint'])
        # registered before the create too: a preview made by a command that then failed is found by this run's origin port
        cleanups.append(('preview of port %d' % port, lambda: 'preview' in gone or remove_previews(s, port, made.get('preview'))))
        _, preview = s.pb('preview', str(port), '--background', '--ttl', '30m')
        made['preview'] = (preview['id'], preview['endpoint'])
        urls = {kind: {'%s on %s' % (kind, edge): url for edge, url in run.addresses(made[kind][1])} for kind in ('preview', 'tunnel')}
        both = {**urls['preview'], **urls['tunnel']}
        up = run.until_served(both, 180)
        for key, (status, ok) in up.items():
            run.log(ok, '%s serves before the cut' % key, 'HTTP %d' % status)
        if not all(ok for _, ok in up.values()):
            return  # nothing to prove by disturbing a staging that does not serve
        address, control_port = s.server_address(), urllib.parse.urlsplit(s.api).port
        inserts = []
        for container, _, _ in s.edges:
            insert, remove = cut_rules(netns[container], control_port, address, tag)
            inserts.append(insert)
            undo.append(remove)
        # The dead man first: whatever happens to this process or its ssh, the staging host removes the rules.
        s.ssh('sudo -n sh -c %s' % shlex.quote('nohup sh -c %s >/dev/null 2>&1 </dev/null &' % shlex.quote('sleep %d; %s' % (DEAD_MAN_SECONDS, '; '.join(undo)))))
        s.ssh('sudo -n sh -c %s' % shlex.quote(' && '.join(inserts)))
        cut_at = run.clock()
        run.log(True, 'the edges are cut off the control server', 'DROP to port %d and %s in %d edge namespace(s)' % (control_port, 'the server container', len(s.edges)))
        run.all_served(run.watch(both, OUTAGE_SECONDS), 'during the control outage')
        for container, _, node in s.edges:
            packets = sum(int(line.split()[0]) for line in s.rules(netns[container]).splitlines() if tag in line)
            _, age = s.heartbeat_age(node)
            run.log(packets > 0 and age >= STALE_AT_CUT_END, 'the cut of %s was real' % container,
                    '%d packets dropped; the server last heard from it %d s ago after %d s' % (packets, age, round(run.clock() - cut_at)))
        restore(s, netns, undo, tag)
        undo.clear()
        run.log(True, 'the control connection is restored')
        run.all_served(run.watch(both, RECOVERY_SECONDS), 'after the control server is back')
        for container, _, node in s.edges:
            healthy, age = s.heartbeat_age(node)
            run.log(healthy and age < 30, '%s is ready and heartbeating again' % container, 'heartbeat %d s old' % age)
        delete_preview(s, made['preview'][0])
        gone.add('preview')
        run.dropped(urls['preview'], urls['tunnel'], 'preview')
        delete_tunnel(s, made['tunnel'][0])
        gone.add('tunnel')
        run.dropped(urls['tunnel'], {}, 'tunnel')
    finally:
        if undo:
            try:
                restore(s, running(s), undo, tag)
            except Exception as error:  # noqa: BLE001 reported with the exact command; the timer on the host still runs
                leftovers.append('edge cut NOT removed by this run (%s); the host timer removes it within %d s, or run as root there: %s' % (
                    str(error)[:120], DEAD_MAN_SECONDS, '; '.join(undo)))
        for what, cleanup in reversed(cleanups):
            try:
                cleanup()
            except Exception as error:  # noqa: BLE001
                leftovers.append('staging %s NOT removed: %s' % (what, str(error)[:150]))
        try:
            s.ssh('rm -f %s/owner && rmdir %s' % (LOCK, LOCK))
        except Exception as error:  # noqa: BLE001
            leftovers.append('staging lock %s NOT removed (remove it when no run is active): %s' % (LOCK, str(error)[:120]))


def running(s):
    """{edge: netns} of the edges that run. A stopped edge's namespace, and the rules in it, are gone: it never blocks the
    restore of the others."""
    spaces = {}
    for container, _, _ in s.edges:
        try:
            spaces[container] = s.netns(container)
        except RuntimeError:
            pass
    return spaces


def remove_previews(s, port, made):
    if made:
        return delete_preview(s, made[0], missing_ok=True)
    _, listed = s.pb('preview', 'list')
    for item in listed.get('items', []):
        if (item.get('target') or {}).get('address') == '127.0.0.1:%d' % port:
            delete_preview(s, item['id'], missing_ok=True)


def restore(s, netns, undo, tag):
    s.ssh('sudo -n sh -c %s' % shlex.quote('; '.join(undo)), check=False)
    for container, space in netns.items():
        if tag in s.rules(space):
            raise RuntimeError('DROP rules of %s are still in %s' % (tag, container))


def delete_preview(s, identifier, missing_ok=False):
    code, value = s.pb('preview', 'delete', identifier, check=False)
    if code and not (missing_ok and 'not_found' in json.dumps(value)):
        raise RuntimeError('pb preview delete %s: exit %d %s' % (identifier, code, json.dumps(value)[:150]))


def delete_tunnel(s, identifier, missing_ok=False):
    """identifier: the tunnel's id, or its name. pb asks for the code its own preview prints."""
    code, value = s.pb('tunnel', 'delete', identifier, check=False)
    token = (value.get('data') or {}).get('confirmation_token')
    if not token:
        if missing_ok and 'not_found' in json.dumps(value):
            return
        raise RuntimeError('pb tunnel delete %s gave no confirmation code: exit %d %s' % (identifier, code, json.dumps(value)[:150]))
    code, value = s.pb('tunnel', 'delete', identifier, '--confirm', token, '--wait', check=False)
    if code:
        raise RuntimeError('pb tunnel delete %s: exit %d %s' % (identifier, code, json.dumps(value)[:150]))


def settings():
    import dev_targets
    value = dev_targets.load()
    return dev_targets.smoke_environment(value), value.get('staging_host', '')


def check(m):
    """The fixture_checks.py row: (ok, detail). m.leftovers receives anything cleanup could not remove."""
    import console_rows
    env, host = settings()
    tag = 'edgeout-' + secrets.token_hex(4)
    run = Run(Staging(env, host), 'pb-%s-marker' % tag)
    try:
        drill(run, tag, m.leftovers)
    except Exception as error:  # noqa: BLE001 a FAIL with the reason, after the drill's own cleanup
        run.log(False, 'the drill stopped', '%s: %s' % (type(error).__name__, str(error)[:200]))
    return console_rows.verdict('staging-edge-outage', [('staging-edge-outage', 1 if run.failed else 0, '\n'.join(run.lines))])
