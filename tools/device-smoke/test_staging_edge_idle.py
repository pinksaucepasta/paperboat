import concurrent.futures
import copy
import hashlib
import json
import os
from pathlib import Path
import re
import socket
import ssl
import struct
import subprocess
import tempfile
import threading
import time
import unittest
import urllib.request
from unittest import mock

import fixture_checks
import idle_streams
import preflight_fixtures
import staging_edge_idle as row

API = 'https://region.stg.example.test:18443'
ENV = {'SMOKE_STAGING_API': API, 'SMOKE_OPERATOR_API': 'https://api.example.test',
       'SMOKE_STAGING_EDGES': 'pb-stg-edge:18446:stg_managed_edge,pb-stg-edge-b:18456:stg_managed_edge_b'}
TAG = 'edgeidle-0011aabb'


class Host:
    """The staging host as the row sees it over ssh: a fake with the edges' limits, both fixtures' pb and the node table."""

    def __init__(self):
        self.control, self.server, self.client_server = API, API, API
        self.limits = {'pb-stg-edge': 120_000_000_000, 'pb-stg-edge-b': 120_000_000_000}
        self.ready = ['stg_managed_edge', 'stg_managed_edge_b']
        self.scripts, self.deleted, self.lock_held, self.route_idle = [], [], False, 90000
        self.published, self.access_log, self.tcp_create_fails, self.delete_fails = True, '{"listen_address":"127.0.0.1:8703"}', False, False

    def __call__(self, argv, **kw):
        script = argv[-1]
        self.scripts.append(script)
        out, code = '', 0
        if 'public_tcp_idle_timeout' in script:
            edge = re.search(r'(pb-stg-edge(?:-b)?)\)', script).group(1)
            out = '%d\n' % self.limits[edge]
        elif 'control_url' in script:
            out = self.control + '\n'
        elif 'config show' in script:
            out = json.dumps({'data': {'server_url': self.client_server if 'pb-stg-b ' in script else self.server}})
        elif "'edge'=any(roles)" in script.replace('\'"\'"\'', "'"):
            out = ''.join(n + '\n' for n in self.ready)
        elif script.startswith('mkdir %s' % row.LOCK):
            code = 1 if self.lock_held else 0
        elif 'sha256sum' in script:
            out = 'ab' * 32 + '  -\n'
        elif 'tunnel create' in script:
            if self.tcp_create_fails and '-tcp ' in script:
                code = 1
            else:
                out = json.dumps({'tunnel': {'id': 'tunnel_1', 'stable_endpoint': 'https://web.pub.stg.example.test'}})
        elif 'tunnel route list' in script:
            protocols = ['tcp_private'] if '-priv' in script else ['tcp', 'tls']
            out = json.dumps({'items': [{'protocol': p, 'idle_timeout_ms': self.route_idle, 'public_tcp_port': 10000 if p == 'tcp' else 443,
                                         'host_match': {'type': 'managed_exact', 'hostname': 'db.pub.stg.example.test'}} for p in protocols]})
        elif 'tunnel route add' in script:
            out = '{"operation": {"state": "succeeded"}}'
        elif 'docker port' in script:
            code, out = (0, '10.0.0.7:10000\n') if self.published else (1, '')
        elif 'access.log' in script and script.count('cat ') and 'pkill' not in script and 'exec timeout' not in script:
            out = self.access_log
        elif 'tunnel delete' in script:
            name = re.search(r'tunnel delete (\S+)', script).group(1)
            if self.delete_fails:
                code, out = 1, '{"error": {"code": "internal"}}'
            elif '--confirm' in script:
                self.deleted.append(name)
                out = '{"tunnel": {"id": "tunnel_1"}}'
            else:
                out, code = '{"data": {"confirmation_token": "ABC123"}}', 1
        return subprocess.CompletedProcess(argv, code, out, '')

    def disturbed(self):
        return [s for s in self.scripts if 'tunnel create' in s or s.startswith('mkdir') or 'idle_streams' in s or 'pb access' in s]


def good(spec):
    result = {'name': spec['name'], 'busy': spec['busy'], 'opened': True, 'opened_utc': '2026-01-01T00:00:00.000+00:00'}
    if spec['busy']:
        return dict(result, closed=None, age=205.1, sent=21, echoed=21, alive_utc='2026-01-01T00:03:25.100+00:00')
    return dict(result, closed='eof', after=120.0, age=120.0, sent=0, echoed=0, closed_utc='2026-01-01T00:02:00.000+00:00')


class Holders:
    def __init__(self, answer=good, private_error=None):
        self.answer, self.private_error, self.plans = answer, private_error, {}

    def public(self, plan):
        self.plans['public'] = plan
        return [self.answer(spec) for spec in plan['streams']]

    def private(self, plan):
        self.plans['private'] = plan
        if self.private_error:
            raise RuntimeError(self.private_error)
        return [self.answer(spec) for spec in plan['streams']]


class Clock:
    def __init__(self):
        self.now = 0.0

    def __call__(self):
        return self.now

    def sleep(self, seconds):
        self.now += seconds


def drill(host, holders=None, env=ENV, https=lambda now: (200, 'marker-x')):
    clock, holders = Clock(), holders or Holders()
    staging = row.Staging(env, 'user@staging', run=host)
    client = copy.copy(staging)
    client.machine = env.get('SMOKE_STAGING_CLIENT', 'pb-stg-b')
    run = row.Run(staging, 'marker-x', sleep=clock.sleep, clock=clock, fetch=lambda url: https(clock.now))
    started = []

    def submit(function, plan):
        """A job that takes 206 s of the fake clock, as the real holders do."""
        class Job:
            def __init__(self):
                self.end = clock.now + 206
                try:
                    self.value, self.error = function(plan), None
                except Exception as error:  # noqa: BLE001 as a future keeps it
                    self.value, self.error = None, error

            def done(self):
                return clock.now >= self.end

            def result(self):
                if self.error:
                    raise self.error
                return self.value
        started.append(Job())
        return started[-1]
    leftovers = []
    with mock.patch('builtins.print'):
        try:
            row.drill(run, client, holders, TAG, leftovers, submit)
        except Exception as error:  # noqa: BLE001 as check() records it
            run.log(False, 'the drill stopped', str(error))
    return run, leftovers, holders


class Drill(unittest.TestCase):
    def assertCleaned(self, host):
        joined = '\n'.join(host.scripts)
        self.assertIn('[i]dle_streams.py origin', joined)
        self.assertIn('rm -f %s/owner && rmdir %s' % (row.LOCK, row.LOCK), host.scripts[-1])

    def test_a_good_run_passes_every_step_and_deletes_everything_it_made(self):
        host = Host()
        run, leftovers, holders = drill(host)
        self.assertEqual(0, run.failed, '\n'.join(run.lines))
        self.assertEqual([], leftovers)
        self.assertEqual({TAG + '-web', TAG + '-tcp', TAG + '-priv'}, set(host.deleted))
        self.assertCleaned(host)
        self.assertTrue(any("pkill -INT -f" in s and '[p]b access tunnel %s-priv ' % TAG in s for s in host.scripts))
        public, private = holders.plans['public']['streams'], holders.plans['private']['streams']
        self.assertEqual([10000, 10000, 18446, 18446], [s['port'] for s in public])
        self.assertEqual(['db.pub.stg.example.test'] * 4, [s['host'] for s in public])
        self.assertEqual(['ab' * 32] * 2, [s['pin'] for s in public if 'sni' in s])
        self.assertEqual(['127.0.0.1'] * 2, [s['host'] for s in private])
        self.assertEqual([False, True] * 3, [s['busy'] for s in public + private])
        self.assertEqual((205, 10), (holders.plans['public']['hold'], holders.plans['public']['every']))
        self.assertIn('8 of 8 answered 200', '\n'.join(run.lines))
        private_create = next(s for s in host.scripts if 'tunnel create %s-priv' % TAG in s)
        self.assertIn('--private', private_create)
        self.assertTrue(all('--duration 1h' in s for s in host.scripts if 'tunnel create' in s))

    def test_one_https_miss_fails_the_https_step_only(self):
        run, _, _ = drill(Host(), https=lambda now: (502, '') if 100 < now < 130 else (200, 'marker-x'))
        self.assertEqual(1, run.failed, '\n'.join(run.lines))
        self.assertTrue(any(line.startswith('FAIL  HTTPS') for line in run.lines))

    def test_a_failed_private_holder_fails_its_streams_and_keeps_the_public_results(self):
        host = Host()
        run, leftovers, _ = drill(host, Holders(private_error='ssh died'))
        self.assertEqual(2, run.failed, '\n'.join(run.lines))
        self.assertEqual(4, sum(1 for l in run.lines if l.startswith('PASS  public') or l.startswith('PASS  TLS')))
        self.assertEqual([], leftovers)

    def test_a_failed_create_still_removes_what_may_exist_and_releases_the_lock(self):
        host = Host()
        host.tcp_create_fails = True
        run, leftovers, _ = drill(host)
        self.assertTrue(run.failed)
        self.assertEqual({TAG + '-web', TAG + '-tcp'}, set(host.deleted))
        self.assertEqual([], leftovers)
        self.assertCleaned(host)

    def test_a_delete_that_fails_is_reported_as_left_behind(self):
        host = Host()
        host.delete_fails = True
        _, leftovers, _ = drill(host)
        self.assertEqual(3, len(leftovers), leftovers)
        self.assertCleaned(host)

    def test_an_unpublished_public_port_and_a_wrong_route_limit_are_failures(self):
        host = Host()
        host.published = False
        run, leftovers, holders = drill(host)
        self.assertTrue(any(l.startswith('FAIL  pb-stg-edge publishes') for l in run.lines))
        self.assertEqual({}, holders.plans)
        self.assertEqual(3, len(host.deleted))
        host = Host()
        host.route_idle = 300000
        run, _, _ = drill(host)
        self.assertTrue(any(l.startswith("FAIL  every route's own idle limit") for l in run.lines))

    def test_a_held_lock_changes_nothing(self):
        host = Host()
        host.lock_held = True
        run, _, _ = drill(host)
        self.assertEqual(1, run.failed)
        self.assertFalse([s for s in host.scripts if 'tunnel create' in s or 'rmdir %s' % row.LOCK in s and s.startswith('rm')])


class Guards(unittest.TestCase):
    def refused(self, host, env=ENV):
        run, _, _ = drill(host, env=env)
        self.assertIn('the drill stopped', run.lines[-1])
        self.assertEqual([], host.disturbed())
        return run.lines[-1]

    def test_every_ready_edge_must_be_listed_and_have_the_short_limit(self):
        host = Host()
        host.limits['pb-stg-edge-b'] = 0
        self.assertIn('pb-stg-edge-b runs with idle limit at the default', self.refused(host))
        host = Host()
        host.limits['pb-stg-edge'] = 3600_000_000_000
        self.assertIn('3600 s', self.refused(host))
        only_a = dict(ENV, SMOKE_STAGING_EDGES='pb-stg-edge:18446:stg_managed_edge')
        self.assertIn('stg_managed_edge_b are not in SMOKE_STAGING_EDGES', self.refused(Host(), only_a))
        host = Host()
        host.ready = ['stg_managed_edge_b']
        self.assertIn('the first listed edge pb-stg-edge is not ready', self.refused(host))

    def test_a_target_that_is_not_staging_is_refused(self):
        self.refused(Host(), dict(ENV, SMOKE_STAGING_API='https://api.example.test:443'))
        self.refused(Host(), dict(ENV, SMOKE_STAGING_CLIENT='canary-a'))
        for attribute in ('control', 'server', 'client_server'):
            host = Host()
            setattr(host, attribute, 'https://api.example.test')
            self.refused(host)

    def test_a_forwarded_first_edge_is_refused(self):
        env = dict(ENV, SMOKE_STAGING_EDGES='pb-stg-edge:18446:stg_managed_edge:10.0.0.7')
        self.assertIn('forward', self.refused(Host(), env))


class Judging(unittest.TestCase):
    def quiet(self, **kw):
        return row.judge(dict(good({'name': 'q', 'busy': False}), **kw))[0]

    def busy(self, **kw):
        return row.judge(dict(good({'name': 'b', 'busy': True}), **kw))[0]

    def test_a_quiet_stream_passes_only_with_a_clean_eof_inside_the_window(self):
        self.assertTrue(self.quiet(after=115.0))
        self.assertTrue(self.quiet(after=130.0))
        for kw in ({'after': 90.1}, {'after': 114.9}, {'after': 130.1}, {'closed': 'rst'}, {'closed': 'error: x'}):
            self.assertFalse(self.quiet(**kw), kw)
        self.assertFalse(self.quiet(closed=None, alive_utc='2026-01-01T00:03:25.000+00:00'))
        self.assertFalse(row.judge({'name': 'q', 'busy': False, 'opened': False, 'error': 'no'})[0])

    def test_a_busy_stream_passes_only_alive_at_200_s_with_every_byte_back(self):
        self.assertTrue(self.busy())
        for kw in ({'age': 199.9}, {'echoed': 20}, {'sent': 19, 'echoed': 19}):
            self.assertFalse(self.busy(**kw), kw)
        self.assertFalse(self.busy(closed='eof', after=3.0, closed_utc='2026-01-01T00:02:00.000+00:00'))


class Outdated(unittest.TestCase):
    def git(self, where, *words):
        subprocess.run(['git', '-C', str(where), '-c', 'user.name=t', '-c', 'user.email=t@example.test', *words], check=True, capture_output=True)

    def commit(self, where, name, text):
        path = Path(where, name)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)
        self.git(where, 'add', name)
        self.git(where, 'commit', '-qm', 'change ' + name)

    def test_a_checkout_behind_origin_main_in_the_guarded_paths_is_refused(self):
        with tempfile.TemporaryDirectory() as root:
            upstream, clone = Path(root, 'up'), Path(root, 'clone')
            subprocess.run(['git', 'init', '-q', '-b', 'main', str(upstream)], check=True)
            self.commit(upstream, 'tools/device-smoke/a.py', '1')
            self.commit(upstream, 'deploy/staging.py', '1')
            subprocess.run(['git', 'clone', '-q', str(upstream), str(clone)], check=True)
            harness = clone / 'tools/device-smoke'
            self.assertIsNone(row.outdated(harness))
            self.commit(upstream, 'server/x.go', '1')
            self.assertIsNone(row.outdated(harness), 'other paths do not age the row')
            self.commit(clone, 'tools/device-smoke/local.py', '1')
            self.assertIsNone(row.outdated(harness), 'a checkout ahead of main is not outdated')
            for name in ('deploy/staging.py', 'tools/device-smoke/b.py'):
                self.commit(upstream, name, 'new ' + name)
                self.assertIn('outdated checkout', row.outdated(harness), name)
            self.git(clone, 'pull', '-q', '--rebase')
            self.assertIsNone(row.outdated(harness))
            self.git(clone, 'remote', 'set-url', 'origin', str(Path(root, 'gone')))
            self.assertIn('cannot be fetched', row.outdated(harness))
        with tempfile.TemporaryDirectory() as plain:
            with mock.patch.dict(os.environ, {'GIT_CEILING_DIRECTORIES': str(Path(plain).parent)}):
                self.assertIn('not in a git checkout', row.outdated(plain))

    def test_the_snapshot_runs_source_directory_is_the_one_checked(self):
        with mock.patch.dict(os.environ, {'PAPERBOAT_SMOKE_SOURCE_DIR': '/somewhere'}):
            self.assertEqual('/somewhere', row.source_directory())

    def test_an_outdated_checkout_stops_the_row_before_the_staging_host_is_touched(self):
        m = mock.Mock(leftovers=[])
        verdicts = []
        with mock.patch.object(row, 'outdated', lambda source: 'outdated checkout: 1 commit'), \
                mock.patch.object(row, 'settings', lambda: (ENV, 'user@staging')), \
                mock.patch.object(row.Staging, 'ssh', side_effect=AssertionError('staging touched')), \
                mock.patch.object(row, 'keep_timeline'), mock.patch('builtins.print'), \
                mock.patch('console_rows.verdict', lambda check, results: verdicts.append(results) or (False, 'x')):
            row.check(m)
        self.assertIn('Refused: outdated checkout', verdicts[0][0][2])


class StreamHolder(unittest.TestCase):
    """The holder against real sockets on this computer."""

    def serve(self, handle):
        listener = socket.socket()
        listener.bind(('127.0.0.1', 0))
        listener.listen()
        self.addCleanup(listener.close)

        def accept():
            connection, _ = listener.accept()
            try:
                handle(connection)
            finally:
                connection.close()
        threading.Thread(target=accept, daemon=True).start()
        return {'name': 's', 'host': '127.0.0.1', 'port': listener.getsockname()[1], 'busy': False}

    def echo_then(self, ending, quiet=0.6):
        def handle(connection):
            connection.sendall(connection.recv(100))
            time.sleep(quiet)
            ending(connection)
        return handle

    def held(self, handle, busy=False, hold=3, every=0.3):
        spec = dict(self.serve(handle), busy=busy)
        return idle_streams.hold_one(spec, hold, every, 5)

    def test_a_fin_is_eof_and_times_from_the_last_data(self):
        result = self.held(self.echo_then(lambda c: c.shutdown(socket.SHUT_RDWR)))
        self.assertEqual('eof', result['closed'], result)
        self.assertTrue(0.4 <= result['after'] <= 2, result)

    def test_a_reset_is_not_an_eof(self):
        def reset(connection):
            connection.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack('ii', 1, 0))
            connection.close()
        self.assertEqual('rst', self.held(self.echo_then(reset))['closed'])

    def echo(self, connection):
        while True:
            data = connection.recv(100)
            if not data:
                return
            connection.sendall(data)

    def test_a_busy_stream_stays_alive_and_gets_every_byte_back(self):
        result = self.held(self.echo, busy=True, hold=1.4, every=0.3)
        self.assertIsNone(result['closed'], result)
        self.assertEqual(result['sent'], result['echoed'])
        self.assertTrue(result['sent'] >= 4 and result['age'] >= 1.4, result)

    def test_a_quiet_stream_that_nobody_closes_is_alive_at_the_end(self):
        result = self.held(self.echo, hold=0.8)
        self.assertEqual((None, 0), (result['closed'], result['sent']), result)

    def test_a_byte_that_never_comes_back_is_not_alive(self):
        def swallow(connection):
            connection.sendall(connection.recv(100))
            while connection.recv(100):
                pass
        with mock.patch.object(idle_streams, 'ECHO_WAIT', 0.5):
            self.assertTrue(self.held(swallow, busy=True, hold=3, every=0.2)['closed'].startswith('stalled'))

    def test_a_route_that_is_not_served_yet_is_retried_until_the_deadline(self):
        clock, calls = Clock(), []

        def dial(spec):
            calls.append(1)
            raise ConnectionRefusedError('refused')
        with self.assertRaisesRegex(OSError, 'not opened in 4 attempts'):
            idle_streams.open_stream({'name': 's'}, 9, dial=dial, sleep=clock.sleep, clock=clock)

    def test_the_real_origin_echoes_passes_tls_through_and_serves_the_marker(self):
        with tempfile.TemporaryDirectory() as directory:
            subprocess.run(['openssl', 'req', '-x509', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:prime256v1', '-nodes', '-days', '1',
                            '-subj', '/CN=origin', '-keyout', directory + '/tls.key', '-out', directory + '/tls.crt'], check=True, capture_output=True)
            pin = hashlib.sha256(ssl.PEM_cert_to_DER_cert(Path(directory, 'tls.crt').read_text())).hexdigest()
            ports = []
            for _ in range(3):
                with socket.socket() as probe:
                    probe.bind(('127.0.0.1', 0))
                    ports.append(probe.getsockname()[1])
            process = subprocess.Popen(['python3', idle_streams.__file__, 'origin', directory, *map(str, ports), 'the-marker', '30'])
            self.addCleanup(process.wait)
            self.addCleanup(process.kill)
            plan = {'hold': 0.8, 'every': 0.3, 'open': 10, 'streams': [
                {'name': 'plain', 'host': '127.0.0.1', 'port': ports[0], 'busy': True},
                {'name': 'tls', 'host': '127.0.0.1', 'port': ports[1], 'busy': False, 'sni': 'a.example.test', 'pin': pin},
                {'name': 'other certificate', 'host': '127.0.0.1', 'port': ports[1], 'busy': False, 'sni': 'a.example.test', 'pin': '0' * 64}]}
            done = subprocess.run(['python3', '-', 'hold', json.dumps(dict(plan, streams=plan['streams'][:2]))],
                                  input=Path(idle_streams.__file__).read_text(), capture_output=True, text=True, timeout=60)
            self.assertEqual([(True, None), (True, None)], [(r['opened'], r['closed']) for r in row.parsed_streams(done)], done.stdout + done.stderr)
            wrong = idle_streams.hold_one(plan['streams'][2], 0.5, 0.3, 0)
            self.assertIn('TLS did not pass through', wrong['error'])
            with urllib.request.urlopen('http://127.0.0.1:%d/' % ports[2], timeout=5) as reply:
                self.assertEqual(b'the-marker', reply.read())


class Wiring(unittest.TestCase):
    def test_the_row_is_a_fixture_check_with_its_preflight_and_a_smoke_line(self):
        self.assertIs(fixture_checks.CHECKS['staging-edge-idle'], row.check)
        self.assertEqual(['staging_stack'], preflight_fixtures.NEEDS['staging-edge-idle'])
        smoke = Path(__file__).with_name('smoke.sh').read_text()
        self.assertIn('\nfixture_check staging-edge-idle\n', smoke)
        self.assertRegex(smoke, r'fixture_ids=\$\(for id in .* staging-edge-idle ')


if __name__ == '__main__':
    unittest.main()
