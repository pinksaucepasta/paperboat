"""Container/routing lifecycle contracts for managed API replacement."""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('rollout', Path(__file__).with_name('rollout-server.py'))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class ServerRolloutTest(unittest.TestCase):
    def test_retired_server_logs_are_kept_and_old_ones_pruned(self):
        logs = self.root / 'rollout-logs'
        logs.mkdir()
        stale = logs / 'paperboat-server-stale.log'
        stale.write_text('old')
        old = module.time.time() - module.LOG_RETENTION_SECONDS - 60
        module.os.utime(stale, (old, old))
        with patch.object(module.subprocess, 'run') as run:
            run.side_effect = lambda args, stdout=None, **kw: stdout.write(b'request log line\n')
            self.rollout.keep_logs('paperboat-server-old')
        self.assertEqual((logs / 'paperboat-server-old.log').read_bytes(), b'request log line\n')
        self.assertFalse(stale.exists())

    def test_prune_keeps_deployed_running_and_two_previous_images(self):
        listing = ('paperboat-server:5 2026-10-23 10:00:00 +0000 UTC\n'
                   'paperboat-server:4 2026-10-23 09:00:00 +0000 UTC\n'
                   'paperboat-server:3 2026-10-23 08:00:00 +0000 UTC\n'
                   'paperboat-server:2 2026-10-23 07:00:00 +0000 UTC\n'
                   'paperboat-server:1 2026-10-23 06:00:00 +0000 UTC\n'
                   'paperboat-server:<none> 2026-10-22 06:00:00 +0000 UTC\n')
        calls = []

        def docker(args, image=None, timeout=120):
            calls.append(args)
            if args[:2] == ['docker', 'ps']:
                return b'paperboat-server:4\npostgres:17\n'
            if args[:3] == ['docker', 'image', 'ls']:
                return listing.encode()
            return b''
        with patch.object(self.rollout, 'run', side_effect=docker):
            self.rollout.prune_images('paperboat-server:5')
        removed = [args[2] for args in calls if args[:2] == ['docker', 'rmi']]
        # 5 deployed, 4 still in use (draining), 3 and 2 the two previous rollback targets.
        self.assertEqual(removed, ['paperboat-server:1'])
        self.assertIn(['docker', 'builder', 'prune', '-af'], calls)

    def test_prune_failure_never_fails_the_rollout(self):
        with patch.object(self.rollout, 'run', side_effect=module.RolloutError('docker down')):
            self.rollout.prune_images('paperboat-server:5')

    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.original = 'api.example.test {\n reverse_proxy server:8080\n}\nget.example.test {\n reverse_proxy server:8080\n}\n'
        (self.root / 'Caddyfile').write_text(self.original)
        (self.root / '.env').write_text('PAPERBOAT_SERVER_IMAGE=old\nUNCHANGED=fixture\n')
        self.rollout = module.Rollout(self.root)
        self.addCleanup(self.rollout.log.close)
        self.image = 'sha256:' + 'a' * 64
        self.candidate = 'paperboat-server-' + 'a' * 16
        self.operations = []
        self.live = False
        self.fail_reload = False
        self.routes = []

    def docker(self, args, image=None, timeout=120):
        self.operations.append(args)
        if args[:4] == ['docker', 'compose', 'ps', '-q']:
            return b'old-id'
        if args[:3] == ['docker', 'image', 'inspect']:
            return json.dumps([{'Id': self.image}]).encode()
        if args[:2] == ['docker', 'inspect']:
            return json.dumps([{'Name': '/paperboat-server-1', 'Image': 'old-image', 'State': {'Running': True}}]).encode()
        if args[:3] == ['docker', 'exec', 'paperboat-server-1'] and args[3] == 'cat':
            text = 'header\n 0: 00000000:1F90 00000000:0000 0A\n'
            if self.live:
                text += ' 1: 0100007F:1F90 0100007F:ABCD 01\n'
            return text.encode()
        if args[:7] == ['docker', 'compose', 'exec', '-T', 'proxy', 'caddy', 'reload']:
            self.routes.append((self.root / 'Caddyfile').read_text())
            if self.fail_reload:
                self.fail_reload = False
                raise module.RolloutError('reload failed')
        return b''

    def test_ready_replacement_precedes_switch_and_old_live_server_is_retained(self):
        self.live = True
        with patch.object(self.rollout, 'run', side_effect=self.docker):
            self.rollout.deploy('new-image')
        ready = next(i for i, a in enumerate(self.operations) if a[:3] == ['docker', 'exec', self.candidate] and 'curl' in a)
        reload = next(i for i, a in enumerate(self.operations) if 'reload' in a)
        self.assertLess(ready, reload)
        self.assertFalse(any(a[:2] == ['docker', 'stop'] for a in self.operations))
        state = json.loads(self.rollout.state_path.read_text())
        self.assertEqual(state['active'], self.candidate)
        self.assertEqual(state['retired'], ['paperboat-server-1'])
        self.assertIn('UNCHANGED=fixture', (self.root / '.env').read_text())
        self.live = False
        with patch.object(self.rollout, 'run', side_effect=self.docker):
            self.rollout.reap(state)
        self.assertEqual(state['retired'], [])
        self.assertIn(['docker', 'stop', '--time', '30', 'paperboat-server-1'], self.operations)

    def test_retired_server_drains_workers_after_the_switch(self):
        with patch.object(self.rollout, 'run', side_effect=self.docker):
            self.rollout.deploy('new-image')
        reload = next(i for i, a in enumerate(self.operations) if 'reload' in a)
        drain = self.operations.index(['docker', 'kill', '--signal=SIGUSR1', 'paperboat-server-1'])
        self.assertLess(reload, drain)

    def test_failed_drain_signal_keeps_the_new_server(self):
        def docker(args, image=None, timeout=120):
            if args[:2] == ['docker', 'kill']:
                self.operations.append(args)
                raise module.RolloutError('no such container')
            return self.docker(args, image, timeout)
        with patch.object(self.rollout, 'run', side_effect=docker):
            self.rollout.deploy('new-image')
        self.assertFalse(any(a[:3] == ['docker', 'rm', '-f'] for a in self.operations))
        self.assertEqual(json.loads(self.rollout.state_path.read_text())['active'], self.candidate)

    def test_failed_readiness_does_not_switch_or_stop_previous_server(self):
        with patch.object(self.rollout, 'run', side_effect=self.docker), patch.object(self.rollout, 'ready', return_value=False), patch.object(module.time, 'sleep'):
            with self.assertRaisesRegex(module.RolloutError, 'did not become ready'):
                self.rollout.deploy('new-image')
        self.assertEqual((self.root / 'Caddyfile').read_text(), self.original)
        self.assertFalse(any('reload' in a or a[:2] == ['docker', 'stop'] for a in self.operations))
        self.assertIn(['docker', 'rm', '-f', self.candidate], self.operations)

    def test_interrupted_switch_restores_route_without_killing_either_server(self):
        self.rollout.save(self.rollout.journal, {'before': {'active': 'paperboat-server-1', 'retired': []}, 'candidate': self.candidate, 'caddy': self.original, 'env': 'PAPERBOAT_SERVER_IMAGE=old\n'})
        def run(args, **kwargs):
            self.operations.append(args)
            return b''
        with patch.object(self.rollout, 'run', side_effect=run):
            self.rollout.recover()
        self.assertEqual((self.root / 'Caddyfile').read_text(), self.original)
        self.assertEqual(json.loads(self.rollout.state_path.read_text())['retired'], [self.candidate])
        self.assertFalse(any(a[:2] in (['docker', 'stop'], ['docker', 'rm']) for a in self.operations))

    def test_reload_failure_restores_old_upstream_and_retains_candidate_connections(self):
        self.fail_reload = True
        with patch.object(self.rollout, 'run', side_effect=self.docker):
            with self.assertRaisesRegex(module.RolloutError, 'reload failed'):
                self.rollout.deploy('new-image')
        self.assertIn(self.candidate, self.routes[0])
        self.assertEqual(self.routes[1], self.original)
        self.assertEqual((self.root / 'Caddyfile').read_text(), self.original)
        self.assertEqual(json.loads(self.rollout.state_path.read_text())['retired'], [self.candidate])
        self.assertFalse(any(a[:2] in (['docker', 'stop'], ['docker', 'rm']) for a in self.operations))

    def test_tcp_retirement_counts_live_ipv4_ipv6_but_not_outbound_or_timewait(self):
        ipv4 = 'header\n0: 00000000:1F90 00000000:0000 0A\n1: 0100007F:1F90 0100007F:AAAA 01\n2: 0100007F:CCCC 0100007F:1538 01\n3: 0100007F:1F90 0100007F:AAAA 06\n'
        ipv6 = 'header\n0: 00000000000000000000000000000000:24E3 00000000000000000000000000000000:0000 0A\n1: 00000000000000000000000000000001:24E3 00000000000000000000000000000002:AAAA 08\n'
        self.assertEqual(module.connection_count([ipv4, ipv6]), 2)


if __name__ == '__main__':
    unittest.main()
