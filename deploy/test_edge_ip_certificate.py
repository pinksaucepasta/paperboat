"""report() names the alert only with a host renewal accepts; anything else pushes nothing and never raises."""
import importlib.util
import json
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


class EdgeIpCertificateReportTest(unittest.TestCase):
    def report(self, doc=MISSING, raw=None, error='boom'):
        with tempfile.TemporaryDirectory() as d:
            if raw is not None or doc is not MISSING:
                (Path(d) / 'tunnel').mkdir()
                text = raw if raw is not None else json.dumps(doc)
                (Path(d) / 'tunnel/deployment.json').write_text(text)
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

    def test_error_capped(self):
        push = self.report({'connector_advertise_host': '8.8.8.8'}, error='x' * 1000)
        self.assertEqual(push.call_args.args[1]['error'], 'x' * 300)

    def test_label_length(self):
        self.assertEqual(edge.subject_flags('a' * 63 + '.com'), ['--domain', 'a' * 63 + '.com'])
        with self.assertRaises(ValueError):
            edge.subject_flags('a' * 64 + '.com')


if __name__ == '__main__':
    unittest.main()
