import json
import re
import shlex
import subprocess
import unittest
from unittest import mock

import fixture_checks
import preflight_fixtures
import staging_edge_outage as row

API = 'https://region.stg.example.test:18443'
ENV = {'SMOKE_STAGING_API': API, 'SMOKE_OPERATOR_API': 'https://api.example.test'}


class Host:
    """The staging host as the row sees it over ssh: a fake with the edge's rules, the fixture's pb and the node table."""

    def __init__(self, control=API, server=API):
        self.control, self.server = control, server
        self.scripts, self.rules, self.deleted, self.lock_held, self.sticky_rules = [], '', [], False, False
        self.preview_fails, self.lock_sticks, self.port = False, False, None

    def __call__(self, argv, **kw):
        script = argv[-1]
        self.scripts.append(script)
        out, code = '', 0
        if 'rmdir' in script and script.startswith('rm -f'):
            code = 1 if self.lock_sticks else 0
        elif 'preview list' in script:
            target = {'address': '127.0.0.1:%s' % self.port}
            out = json.dumps({'items': [{'id': 'preview_lost', 'owner_machine_id': 'machine_fixture', 'target': target},
                                        {'id': 'preview_theirs', 'owner_machine_id': 'machine_other', 'target': target},
                                        {'id': 'preview_other', 'owner_machine_id': 'machine_fixture', 'target': {'address': '127.0.0.1:1'}}]})
        elif 'control_url' in script:
            out = self.control + '\n'
        elif 'config show' in script:
            out = json.dumps({'data': {'server_url': self.server}})
        elif script.startswith('mkdir %s' % row.LOCK):
            code = 1 if self.lock_held else 0
        elif 'State.Pid' in script:
            out = '4242\n'
        elif 'iptables -L OUTPUT' in script:
            out = self.rules
        elif 'psql' in script:
            out = 't|t|0\n'.replace('t|t', 'ready|t') if 'DROP' not in self.rules else 'ready|t|95\n'
        elif 'NetworkSettings' in script:
            out = '10.0.0.3 \n'
        elif 'nohup' in script:
            pass
        elif 'iptables -I' in script:
            tag = re.search(r'edgeout-[0-9a-f]+', script).group(0)
            self.rules = '  7 420 DROP tcp -- * * 0.0.0.0/0 0.0.0.0/0 /* %s */\n' % tag
        elif 'iptables -D' in script:
            if not self.sticky_rules:
                self.rules = ''
        elif 'tunnel create' in script:
            out = json.dumps({'tunnel': {'id': 'tunnel_1', 'stable_endpoint': 'https://a.pub.stg.example.test', 'created_by_host_id': 'machine_fixture'}})
        elif '.local/bin/pb preview delete' in script:
            self.deleted.append('preview')
            out = '{"id": "preview_1"}'
        elif '.local/bin/pb preview' in script and self.preview_fails:
            self.port = re.search(r'pb preview (\d+)', script).group(1)
            code = 1
        elif '.local/bin/pb preview' in script:
            out = json.dumps({'id': 'preview_1', 'endpoint': 'https://b.pub.stg.example.test'})
        elif 'tunnel delete' in script:
            if '--confirm' in script:
                self.deleted.append('tunnel')
                out = '{"tunnel": {"id": "tunnel_1"}}'
            elif 'tunnel' in self.deleted:
                out, code = '{"error": {"code": "not_found"}}', 1
            else:
                out, code = '{"data": {"confirmation_token": "ABC123"}}', 1
        return subprocess.CompletedProcess(argv, code, out, '')

    def index(self, needle):
        return next(i for i, s in enumerate(self.scripts) if needle in s)

    def disturbed(self):
        return [s for s in self.scripts if 'iptables -I' in s or 'tunnel create' in s or s.startswith('mkdir')]


class Clock:
    def __init__(self):
        self.now = 0.0

    def __call__(self):
        return self.now

    def sleep(self, seconds):
        self.now += seconds


def drill(host, fetch=None, env=ENV, forwards=None):
    clock = Clock()

    def served(url):
        kind = 'tunnel' if '//a.' in url else 'preview'
        return (404, '') if kind in host.deleted else (200, 'marker-x')

    staging = row.Staging(env, 'user@staging', run=host)
    forwards = [] if forwards is None else forwards

    def forward(address, port):
        forwards.append(('open', address, port))
        return 40000 + port % 1000, lambda: forwards.append(('closed', address, port))

    staging.forward = forward
    run = row.Run(staging, 'marker-x', sleep=clock.sleep, clock=clock, fetch=fetch(host, clock, served) if fetch else served)
    leftovers = []
    with mock.patch('builtins.print'):
        try:
            row.drill(run, 'edgeout-0011aabb', leftovers)
        except Exception as error:  # noqa: BLE001 as check() records it
            run.log(False, 'the drill stopped', str(error))
    return run, leftovers


class Guards(unittest.TestCase):
    def refused(self, host, env):
        with self.assertRaises(row.Refused):
            row.Staging(env, 'user@staging', run=host).guard()
        self.assertEqual([], host.disturbed())

    def test_a_production_or_non_staging_api_is_refused_before_anything_changes(self):
        for api in ('https://api.example.test:443', 'https://stgapi.example.test:18443', 'http://region.stg.example.test:18443', 'https://region.stg.example.test', ''):
            self.refused(Host(control=api, server=api), dict(ENV, SMOKE_STAGING_API=api))
        self.refused(Host(), dict(ENV, SMOKE_OPERATOR_API='https://region.stg.example.test'))

    def test_an_edge_or_fixture_that_reports_elsewhere_is_refused(self):
        self.refused(Host(control='https://api.example.test'), ENV)
        self.refused(Host(server='https://api.example.test'), ENV)

    def test_only_staging_named_containers_are_touched(self):
        self.refused(Host(), dict(ENV, SMOKE_STAGING_EDGES='pb-edge:443:stg_managed_edge'))
        self.refused(Host(), dict(ENV, SMOKE_STAGING_EDGES="pb-stg-edge:18446:x';drop"))
        self.refused(Host(), dict(ENV, SMOKE_STAGING_MACHINE='canary-a'))

    def test_the_rules_are_tagged_and_the_undo_removes_only_them(self):
        insert, undo = row.cut_rules('nsenter -t 4242 -n', 18443, '10.0.0.3', 'edgeout-1')
        self.assertEqual(2, insert.count('--comment edgeout-1 -j DROP'))
        self.assertEqual(2, undo.count('--comment edgeout-1 -j DROP'))
        self.assertNotIn(' -F', undo)


class Fetch(unittest.TestCase):
    def test_a_dial_address_keeps_the_name_for_the_certificate_and_host(self):
        seen = []

        def refuse(address, timeout):
            seen.append(address)
            raise OSError('closed')

        with mock.patch.object(row.socket, 'create_connection', refuse):
            self.assertEqual((0, ''), row.fetch('https://a.pub.stg.example.test:18456/#dial=127.0.0.1:40456'))
            row.fetch('https://a.pub.stg.example.test:18446/')
        self.assertEqual([('127.0.0.1', 40456), ('a.pub.stg.example.test', 18446)], seen)

    def test_an_edge_with_a_dial_address_gets_it_on_its_urls_only(self):
        env = dict(ENV, SMOKE_STAGING_EDGES='pb-stg-edge:18446:stg_a,pb-stg-edge-b:18456:stg_b:10.0.0.7')
        run = row.Run(row.Staging(env, 'u@h', run=Host()), 'm')
        run.s.local['pb-stg-edge-b'] = 40456
        self.assertEqual([('pb-stg-edge', 'https://x.pub.stg.example.test:18446/'), ('pb-stg-edge-b', 'https://x.pub.stg.example.test:18456/#dial=127.0.0.1:40456')],
                         run.addresses('https://x.pub.stg.example.test'))
        with self.assertRaises(row.Refused):
            row.Staging(dict(ENV, SMOKE_STAGING_EDGES='pb-stg-edge:1:stg_a:evil.example'), 'u@h', run=Host()).guard()


class Forward(unittest.TestCase):
    def test_the_forward_lasts_the_run_and_is_closed_even_when_the_drill_crashes(self):
        env = dict(ENV, SMOKE_STAGING_EDGES='pb-stg-edge:18446:stg_a,pb-stg-edge-b:18456:stg_b:10.0.0.7')
        forwards = []
        run, leftovers = drill(Host(), env=env, forwards=forwards)
        self.assertEqual((0, []), (run.failed, leftovers), run.lines)
        self.assertEqual([('open', '10.0.0.7', 18456), ('closed', '10.0.0.7', 18456)], forwards)

        def crash(host, clock, served):
            def fetch(url):
                raise KeyboardInterrupt
            return fetch
        forwards = []
        with self.assertRaises(KeyboardInterrupt):
            drill(Host(), crash, env=env, forwards=forwards)
        self.assertEqual('closed', forwards[-1][0])

    def test_a_forward_takes_a_free_local_port_and_kills_an_ssh_that_ignores_the_stop(self):
        commands = []
        process = mock.Mock(**{'poll.return_value': None, 'wait.side_effect': [subprocess.TimeoutExpired('ssh', 15), 0]})

        def popen(argv, **kw):
            commands.append(argv)
            return process

        with mock.patch.object(row.socket, 'create_connection'), mock.patch.object(row.time, 'sleep'):
            local, stop = row.Staging(ENV, 'u@h', run=Host()).forward('10.0.0.7', 18456, popen=popen)
        self.assertNotEqual(18456, local)
        self.assertIn('127.0.0.1:%d:10.0.0.7:18456' % local, commands[0])
        stop()
        process.kill.assert_called_once()

    def test_a_forward_that_never_listens_is_an_error_and_its_process_is_stopped(self):
        process = mock.Mock(**{'poll.return_value': 1})
        with self.assertRaises(RuntimeError):
            row.Staging(ENV, 'u@h', run=Host()).forward('10.0.0.7', 18456, popen=lambda *a, **k: process, ready=1)
        process.terminate.assert_called_once()


class EveryEdge(unittest.TestCase):
    TWO = dict(ENV, SMOKE_STAGING_EDGES='pb-stg-edge:18446:stg_managed_edge,pb-stg-edge-b:18456:stg_managed_edge_b')

    def test_an_edge_that_does_not_serve_fails_the_row_and_the_others_are_still_drilled(self):
        def no_b(host, clock, served):
            return lambda url: (0, '') if ':18456/' in url else served(url)
        host = Host()
        run, leftovers = drill(host, no_b, env=self.TWO)
        failed = [line for line in run.lines if line.startswith('FAIL')]
        self.assertEqual(2, len(failed), failed)
        self.assertTrue(all('pb-stg-edge-b serves before the cut' in line for line in failed), failed)
        self.assertTrue(any('during the control outage' in line and line.startswith('PASS') for line in run.lines))
        self.assertEqual((['preview', 'tunnel'], '', []), (host.deleted[:2], host.rules, leftovers))

    def test_both_edges_serving_are_both_cut_and_checked(self):
        host = Host()
        run, _ = drill(host, env=self.TWO)
        self.assertEqual(0, run.failed, run.lines)
        self.assertEqual(4, sum('during the control outage' in line for line in run.lines))
        self.assertEqual(2, sum('the cut of pb-stg-edge' in line for line in run.lines))


class OriginCleanup(unittest.TestCase):
    def test_pkill_matches_the_origin_and_not_the_shell_that_runs_the_cleanup(self):
        command = row.origin_cleanup(8765, '/tmp/edgeout-1')
        pattern = shlex.split(command.split(';')[0])[-1]
        self.assertTrue(re.search(pattern, 'python3 -m http.server 8765 --bind 127.0.0.1 --directory /tmp/edgeout-1'))
        self.assertFalse(re.search(pattern, 'sh -c ' + command))


class Drill(unittest.TestCase):
    def test_a_healthy_run_passes_and_restores_and_removes_everything(self):
        host = Host()
        run, leftovers = drill(host)
        self.assertEqual((0, []), (run.failed, leftovers), run.lines)
        self.assertEqual('', host.rules)
        self.assertEqual(['preview', 'tunnel'], host.deleted[:2])
        self.assertTrue(any('rmdir %s' % row.LOCK in s for s in host.scripts))

    def test_the_host_timer_is_installed_before_the_rules(self):
        host = Host()
        drill(host)
        timer = host.index('nohup')
        self.assertIn('sleep %d' % row.DEAD_MAN_SECONDS, host.scripts[timer])
        self.assertIn('iptables -D', host.scripts[timer])
        self.assertLess(timer, host.index('iptables -I'))

    def test_a_miss_during_the_outage_fails_and_the_rest_still_runs(self):
        def flaky(host, clock, served):
            def fetch(url):
                return (502, '') if 'DROP' in host.rules and '//a.' in url else served(url)
            return fetch
        host = Host()
        run, _ = drill(host, flaky)
        failed = [line for line in run.lines if line.startswith('FAIL')]
        self.assertEqual(1, len(failed), failed)
        self.assertIn('during the control outage', failed[0])
        self.assertEqual('', host.rules)

    def test_an_edge_that_stops_answering_is_not_a_drop(self):
        def silent(host, clock, served):
            return lambda url: (0, '') if 'preview' in host.deleted and '//b.' in url else served(url)
        run, _ = drill(Host(), silent)
        self.assertTrue(any(line.startswith('FAIL  deleting the preview drops it') for line in run.lines), run.lines)

    def test_a_crash_during_the_cut_still_restores_the_edge_and_removes_what_was_made(self):
        def crash(host, clock, served):
            def fetch(url):
                if 'DROP' in host.rules:
                    raise KeyboardInterrupt
                return served(url)
            return fetch
        host = Host()
        with self.assertRaises(KeyboardInterrupt):
            drill(host, crash)
        self.assertEqual('', host.rules)
        self.assertEqual({'preview', 'tunnel'}, set(host.deleted))

    def test_rules_that_stay_are_reported_with_the_command(self):
        host = Host()
        host.sticky_rules = True
        run, leftovers = drill(host)
        self.assertTrue(run.failed)
        self.assertTrue(any('edge cut NOT removed' in text and 'iptables -D' in text for text in leftovers), leftovers)

    def test_nothing_is_disturbed_when_urls_never_serve_or_the_lock_is_held(self):
        host = Host()
        run, _ = drill(host, lambda *a: (lambda url: (502, '')))
        self.assertTrue(run.failed)
        self.assertFalse([s for s in host.scripts if 'iptables -I' in s])
        held = Host()
        held.lock_held = True
        run, _ = drill(held)
        self.assertTrue(run.failed)
        self.assertFalse([s for s in held.scripts if 'iptables' in s or 'tunnel create' in s])
        self.assertFalse([s for s in held.scripts if s.startswith('rm -f')])  # someone else's lock is not released


class Cleanups(unittest.TestCase):
    def test_a_preview_made_by_a_failed_command_is_found_by_the_runs_port_and_deleted(self):
        host = Host()
        host.preview_fails = True
        run, leftovers = drill(host)
        self.assertTrue(run.failed)
        self.assertEqual([], leftovers)
        deletes = [s for s in host.scripts if 'pb preview delete' in s]
        self.assertEqual(1, len(deletes))
        self.assertIn('preview_lost', deletes[0])

    def test_a_lock_that_stays_is_reported(self):
        host = Host()
        host.lock_sticks = True
        _, leftovers = drill(host)
        self.assertTrue(any('lock' in text and 'NOT removed' in text for text in leftovers), leftovers)

    def test_a_stopped_edge_does_not_block_the_restore_of_the_others(self):
        class Two:
            edges = [('pb-stg-edge', 1, 'stg_a'), ('pb-stg-edge-b', 2, 'stg_b')]

            def netns(self, container):
                if container == 'pb-stg-edge':
                    raise RuntimeError('edge pb-stg-edge is not running')
                return 'nsenter -t 7 -n'

        self.assertEqual({'pb-stg-edge-b': 'nsenter -t 7 -n'}, row.running(Two()))


class Timeline(unittest.TestCase):
    def test_it_holds_every_sample_and_a_write_that_fails_never_replaces_the_result(self):
        import tempfile
        from pathlib import Path
        run, _ = drill(Host())
        self.assertTrue(any('\t200\tTrue' in line for line in run.samples))
        self.assertTrue(any('restoring: removing the DROP rules' in line for line in run.samples))
        with tempfile.TemporaryDirectory() as directory, mock.patch('results_dir.results_dir', lambda *_: Path(directory)), mock.patch('builtins.print'):
            row.keep_timeline(run, 'edgeout-1')
            written = list(Path(directory, 'fixture-checks').glob('*.tsv'))
            self.assertEqual(1, len(written))
            self.assertEqual(0o600, written[0].stat().st_mode & 0o777)
        with mock.patch('results_dir.results_dir', side_effect=OSError('read-only')), mock.patch('builtins.print'):
            row.keep_timeline(run, 'edgeout-1')  # no raise


class Registration(unittest.TestCase):
    def test_the_row_runs_in_smoke_and_is_preflighted(self):
        smoke = (row.__file__.replace('staging_edge_outage.py', 'smoke.sh'))
        with open(smoke) as handle:
            text = handle.read()
        self.assertIs(row.check, fixture_checks.CHECKS['staging-edge-outage'])
        self.assertIn('\nfixture_check staging-edge-outage\n', text)
        self.assertEqual(['staging_stack'], preflight_fixtures.NEEDS['staging-edge-outage'])


if __name__ == '__main__':
    unittest.main()
