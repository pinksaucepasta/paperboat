import datetime as dt
import json
from pathlib import Path
import subprocess
import tempfile
import unittest

import last_green
from preview_receipt import receipt_timestamp


class LastGreenTests(unittest.TestCase):
    def test_rfc3339_timestamp_fraction_and_offset(self):
        for digits in range(1, 10):
            fraction = '303661234'[:digits]
            micros = int((fraction + '000000')[:6])
            for offset, minutes in [('Z', 0), ('+00:00', 0), ('+05:30', 330), ('-07:45', -465)]:
                value = '2026-10-18T00:36:06.' + fraction + offset
                with self.subTest(value=value):
                    expected = dt.datetime(2026, 10, 18, 0, 36, 6, micros,
                                           tzinfo=dt.timezone(dt.timedelta(minutes=minutes)))
                    actual = last_green.timestamp(Path('unused.json'), {'at': value})
                    self.assertEqual(expected, actual)
                    self.assertEqual(expected.timestamp(), actual.timestamp())
                    parsed = receipt_timestamp(value)
                    self.assertEqual(expected.utcoffset(), parsed.utcoffset())
                    self.assertEqual(expected.microsecond, parsed.microsecond)
        for value in ['2026-10-18T00:36:06.', '2026-02-30T00:36:06Z',
                      '2026-10-18T00:36:06.30366xZ', 'not-a-timestamp',
                      '2026-10-18T00:36:06.1234567890Z', '2026-10-18T00:36:06+05:99',
                      '2026-10-18T00:36:06', '2026-10-18T24:36:06Z']:
            with self.subTest(invalid=value), self.assertRaises(ValueError):
                last_green.timestamp(Path('unused.json'), {'at': value})

    def receipt(self, root, stamp, version, rows, **extra):
        path = root / (stamp + '.json')
        path.write_text(json.dumps(dict(expect_version=version, results=[dict(id=i, status=s) for i, s in rows], **extra)))
        return path

    def test_exact_check_uses_newest_earlier_pass_not_future_or_other_device(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.receipt(root, '20261016T080000Z', 'dev-1111111', [('preview:hp', 'PASS')])
            wanted = self.receipt(root, '20261016T090000Z', 'dev-2222222', [('preview:hp', 'PASS')])
            self.receipt(root, '20261016T100000Z', 'dev-3333333', [('preview:apple', 'PASS')])
            self.receipt(root, '20261016T120000Z', 'dev-4444444', [('preview:hp', 'PASS')])
            (root / 'broken.json').write_text('{')
            before = dt.datetime(2026, 10, 16, 11, tzinfo=dt.timezone.utc)
            self.assertEqual(wanted, last_green.last_pass(root, 'preview:hp', before)[1])
            self.assertIsNone(last_green.last_pass(root, 'ssh:hp', before))

    def test_report_uses_build_commit_scopes_history_and_names_deploy_precision(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            def git(*args):
                return subprocess.check_output(['git', '-C', str(root), *args], text=True).strip()
            git('init', '-q')
            git('config', 'user.name', 'Fixture')
            git('config', 'user.email', 'fixture@example.test')
            (root / 'cli').mkdir()
            (root / 'cli/main.go').write_text('first')
            git('add', '.'); git('commit', '-qm', 'accepted runtime')
            green = git('rev-parse', 'HEAD')
            (root / 'cli/main.go').write_text('second')
            git('add', '.'); git('commit', '-qm', 'runtime candidate')
            candidate = git('rev-parse', 'HEAD')
            (root / 'server').mkdir()
            (root / 'server/service.go').write_text('producer')
            git('add', '.'); git('commit', '-qm', 'server-only candidate')
            (root / 'PLAN.md').write_text('2026-10-16 | Lane B | server18.6 deployed after gate\n2026-10-18 | Lane B | server18.7 deployed\n')
            results = root / 'results'; results.mkdir()
            self.receipt(results, '20261016T080000Z', 'dev-' + green[:7], [('preview-manage:hp-hp', 'PASS')], source_commit=candidate, server='18.5')
            failed = self.receipt(results, '20261017T080000Z', 'dev-' + candidate[:7], [('preview-manage:hp-hp', 'FAIL'), ('new:hp', 'FAIL')], server='18.6')
            output = last_green.report(failed, results, root)
            self.assertIn('Build commit: ' + green, output)
            self.assertIn('runtime candidate', output)
            self.assertNotIn('server-only candidate', output)
            self.assertIn('git bisect start', output)
            self.assertIn('--only-check preview-manage:hp-hp', output)
            self.assertIn('--targets hp', output)
            self.assertIn('server18.6 deployed', output)
            self.assertIn('server18.7 deployed', output)
            self.assertIn('timezone uncertainty', output)
            self.assertIn('date precision', output)
            self.assertIn('new:hp: no earlier PASS', output)

    @unittest.skipUnless((last_green.ROOT / 'tools/device-smoke/results/20261016T082359Z.json').exists(), 'private device receipt archive is absent')
    def test_real_recorded_browser_receipt_recovers_accepted_build(self):
        directory = last_green.ROOT / 'tools/device-smoke/results'
        before = dt.datetime(2026, 10, 16, 8, 24, tzinfo=dt.timezone.utc)
        previous = last_green.last_pass(directory, 'preview-browser:hp-foreground', before)
        self.assertEqual('20261016T082359Z.json', previous[1].name)
        self.assertEqual('7a5b2e077bb0abe9d2da06b6b562cd262d578c58', last_green.build_commit(previous[2]))


if __name__ == '__main__':
    unittest.main()
