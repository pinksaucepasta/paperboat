"""report() writes the check where the edge's heartbeat reads it: ok, the capped reason and the check time; it never raises."""
import importlib.util
import json
import os
import stat
import tempfile
import unittest
from datetime import datetime, timezone
from pathlib import Path
from types import SimpleNamespace

spec = importlib.util.spec_from_file_location('edge', Path(__file__).with_name('edge-ip-certificate.py'))
edge = importlib.util.module_from_spec(spec)
spec.loader.exec_module(edge)


class EdgeIpCertificateReportTest(unittest.TestCase):
    def test_writes_the_status_the_heartbeat_carries(self):
        with tempfile.TemporaryDirectory() as d:
            edge.report(SimpleNamespace(state_dir=d), False, 'x' * 1000)
            path = Path(d) / 'public-acme/renew-status.json'
            status = json.loads(path.read_text())
            self.assertEqual(stat.S_IMODE(os.stat(path).st_mode), 0o600)
            self.assertFalse(status['ok'])
            self.assertEqual(status['error'], 'x' * 300)
            checked = datetime.strptime(status['checked_at'], '%Y-%m-%dT%H:%M:%SZ').replace(tzinfo=timezone.utc)
            self.assertLess(abs((datetime.now(timezone.utc) - checked).total_seconds()), 60)
            edge.report(SimpleNamespace(state_dir=d), True, '')  # a success replaces the failure: the alert clears
            self.assertEqual({k: v for k, v in json.loads(path.read_text()).items() if k != 'checked_at'}, {'ok': True, 'error': ''})
            self.assertEqual(sorted(p.name for p in path.parent.iterdir()), ['renew-status.json'])

    def test_unwritable_state_never_raises(self):
        with tempfile.TemporaryDirectory() as d:
            (Path(d) / 'public-acme').write_text('not a directory')
            edge.report(SimpleNamespace(state_dir=d), False, 'boom')


class ReadStateTest(unittest.TestCase):
    def test_small_regular_files_only(self):
        with tempfile.TemporaryDirectory() as d:
            (Path(d) / 'tunnel').mkdir()
            (Path(d) / 'tunnel/deployment.json').write_text('{}')
            self.assertEqual(edge.read_state(d, 'tunnel/deployment.json'), '{}')
            (Path(d) / 'tunnel/big').write_text('x' * (edge.MAX_STATE_FILE + 1))
            os.symlink(Path(d) / 'tunnel/deployment.json', Path(d) / 'tunnel/link')
            os.mkfifo(Path(d) / 'tunnel/fifo')
            os.symlink(Path(d) / 'tunnel', Path(d) / 'linkdir')
            for name in ['tunnel/big', 'tunnel/link', 'tunnel/fifo', 'linkdir/deployment.json']:
                with self.subTest(name=name), self.assertRaises((OSError, ValueError)):
                    edge.read_state(d, name)


class SubjectTest(unittest.TestCase):
    def test_label_length(self):
        self.assertEqual(edge.subject_flags('a' * 63 + '.com'), ['--domain', 'a' * 63 + '.com'])
        with self.assertRaises(ValueError):
            edge.subject_flags('a' * 64 + '.com')


if __name__ == '__main__':
    unittest.main()
