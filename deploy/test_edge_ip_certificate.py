"""report() names the alert with a host renewal accepts, else with the edge's node id; neither: pushes nothing, never raises."""
import importlib.util
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

sys.path.insert(0, str(Path(__file__).parent))  # edge-ip-certificate.py imports health_report from its own directory
spec = importlib.util.spec_from_file_location('edge', Path(__file__).with_name('edge-ip-certificate.py'))
edge = importlib.util.module_from_spec(spec)
spec.loader.exec_module(edge)

MISSING = object()
UUID = '0b6f1c1e-2f3a-4c5d-8e9f-0123456789ab'
NODE = 'node_' + UUID  # enrollment's format


class EdgeIpCertificateReportTest(unittest.TestCase):
    def report(self, doc=MISSING, raw=None, error='boom', runtime=MISSING):
        with tempfile.TemporaryDirectory() as d:
            (Path(d) / 'tunnel').mkdir()
            if raw is not None or doc is not MISSING:
                text = raw if raw is not None else json.dumps(doc)
                (Path(d) / 'tunnel/deployment.json').write_text(text)
            if runtime is not MISSING:
                (Path(d) / 'tunnel/runtime.args').write_text(runtime if isinstance(runtime, str) else json.dumps(runtime))
            with mock.patch.object(edge.health_report, 'push') as push:
                edge.report(SimpleNamespace(state_dir=d), False, error)
            return push

    def host(self, host):
        return self.report({'connector_advertise_host': host})

    def test_valid_hosts_push(self):
        for given, sent in [('8.8.8.8', '8.8.8.8'), ('example.com', 'example.com'),
                            ('Example.COM', 'example.com'), ('2001:4860:0:0::8888', '2001:4860::8888')]:
            push = self.host(given)
            push.assert_called_once_with('edge-certificate:' + sent,
                                         {'ok': False, 'error': 'boom', 'stale_after_seconds': 86400})

    def test_invalid_hosts_push_nothing(self):
        bad = ['', ' ', 'a' * (1 << 20), 'a.' * 126 + 'ab', 'a' * 64 + '.com', 'a\x00b', 'a\nb', 'a\r\nb',
               'a;rm', '$(x)', '../etc', 'exämple.com', 'еxample.com', 'example.com.',
               'example.com:443', 'https://u:p@example.com', '[8.8.8.8]', '[2001:4860::8888]', 'a"b',
               'a_b', 'a/b', 'a@b', '-', '.', ':', '10.0.0.1', '127.0.0.1', '192.0.2.1', '::1', '2001:4860::8888%eth0',
               134744072, None, True, 1.5, ['8.8.8.8']]
        for host in bad:
            with self.subTest(host=repr(host)[:40]):
                self.host(host).assert_not_called()

    def test_unreadable_documents_push_nothing(self):
        self.report().assert_not_called()
        self.report(raw='{').assert_not_called()
        self.report({}).assert_not_called()
        self.report(['8.8.8.8']).assert_not_called()
        self.report('8.8.8.8').assert_not_called()
        self.report(raw='[' * 100000).assert_not_called()

    def test_bad_host_alerts_by_node_id(self):
        args = ['-node-id', NODE, '-edge-pool', 'p']
        for doc in [{'connector_advertise_host': 'a;rm'}, {'connector_advertise_host': None}, {}, MISSING]:
            with self.subTest(doc=doc):
                push = self.report(doc, runtime=args, error='host a;rm failed ' + 'x' * 1000)
                push.assert_called_once_with('edge-certificate:node:' + UUID, {
                    'ok': False, 'error': 'the advertised connector host is missing or invalid; fix tunnel/deployment.json',
                    'stale_after_seconds': 86400})
        for runtime in [['--node-id', NODE], ['-node-id=' + NODE], ['-relay-name', '-node-id', '-node-id', NODE]]:
            with self.subTest(runtime=runtime):
                push = self.report({}, runtime=runtime)
                self.assertEqual(push.call_args.args[0], 'edge-certificate:node:' + UUID)
        push = self.report(raw='{', runtime=args)
        self.assertEqual(push.call_args.args[0], 'edge-certificate:node:' + UUID)

    def test_bad_host_without_usable_node_id_pushes_nothing(self):
        for runtime in [MISSING, '{', ['-node-id'], ['-edge-pool', 'p'], ['-node-id', 'edge-01'], ['-node-id', 'Node_' + UUID],
                        ['-node-id', NODE.upper()], ['-node-id', NODE + 'a'], ['-node-id', NODE + '\n'], ['-node-id', '../x'],
                        ['-node-id', 5], ['-node-id', NODE, '-node-id', NODE], ['-node-id', NODE, '--node-id=' + NODE],
                        ['-node-id=' + NODE, '-node-id', 'node_x'], ['-node-id', NODE, 5], ['x', '-node-id', NODE], ['---node-id', NODE], ['-bogus', 'v', '-node-id', NODE],
                        {'-node-id': NODE}, 'x-node-id abc', '[' * 100000]:
            with self.subTest(runtime=repr(runtime)[:40]):
                self.report({'connector_advertise_host': 'a;rm'}, runtime=runtime).assert_not_called()

    def test_symlinked_state_dir_pushes_nothing(self):
        with tempfile.TemporaryDirectory() as d:
            (Path(d) / 'real/tunnel').mkdir(parents=True)
            (Path(d) / 'real/tunnel/runtime.args').write_text(json.dumps(['-node-id', NODE]))
            (Path(d) / 'real/tunnel/deployment.json').write_text(json.dumps({'connector_advertise_host': '8.8.8.8'}))
            (Path(d) / 'link').symlink_to(Path(d) / 'real')
            for state, calls in [('real', 1), ('link', 0), ('link/', 0)]:
                with mock.patch.object(edge.health_report, 'push') as push:
                    edge.report(SimpleNamespace(state_dir=d + '/' + state), False, 'x')
                self.assertEqual(push.call_count, calls, state)

    def test_unsafe_state_files_push_nothing(self):
        bad = {'connector_advertise_host': 'a;rm'}
        self.report(bad, runtime=json.dumps(['-node-id', NODE]) + ' ' * (64 << 10)).assert_not_called()
        self.report(raw=json.dumps({'connector_advertise_host': '8.8.8.8'}) + ' ' * (64 << 10)).assert_not_called()
        with tempfile.TemporaryDirectory() as d, tempfile.TemporaryDirectory() as other:
            (Path(other) / 'runtime.args').write_text(json.dumps(['-node-id', NODE]))
            (Path(other) / 'deployment.json').write_text(json.dumps({'connector_advertise_host': '8.8.8.8'}))
            (Path(d) / 'tunnel').symlink_to(other)  # symlinked directory
            with mock.patch.object(edge.health_report, 'push') as push:
                edge.report(SimpleNamespace(state_dir=d), False, 'x')
            push.assert_not_called()
        with tempfile.TemporaryDirectory() as d, tempfile.TemporaryDirectory() as other:
            (Path(d) / 'tunnel').mkdir()
            (Path(other) / 'runtime.args').write_text(json.dumps(['-node-id', NODE]))
            (Path(d) / 'tunnel/runtime.args').symlink_to(Path(other) / 'runtime.args')  # symlinked file
            os.mkfifo(Path(d) / 'tunnel/deployment.json')  # a FIFO must not stall
            with mock.patch.object(edge.health_report, 'push') as push:
                edge.report(SimpleNamespace(state_dir=d), False, 'x')
            push.assert_not_called()

    def test_valid_host_ignores_node_id(self):
        push = self.report({'connector_advertise_host': 'Example.COM'}, runtime=['-node-id', NODE])
        push.assert_called_once_with('edge-certificate:example.com', {'ok': False, 'error': 'boom', 'stale_after_seconds': 86400})

    def test_error_capped(self):
        push = self.report({'connector_advertise_host': '8.8.8.8'}, error='x' * 1000)
        self.assertEqual(push.call_args.args[1]['error'], 'x' * 300)

    def test_label_length(self):
        self.assertEqual(edge.subject_flags('a' * 63 + '.com'), ['--domain', 'a' * 63 + '.com'])
        with self.assertRaises(ValueError):
            edge.subject_flags('a' * 64 + '.com')


if __name__ == '__main__':
    unittest.main()
