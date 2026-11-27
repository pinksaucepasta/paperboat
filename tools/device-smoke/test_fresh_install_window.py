import datetime
import hashlib
import json
import os
import unittest
import zoneinfo
from unittest import mock

import fixture_checks as f


class FreshInstallWindow(unittest.TestCase):
    def test_the_zone_has_its_window_well_behind_and_ahead(self):
        for hour in range(24):
            now = datetime.datetime(2026, 11, 27, hour, 30, tzinfo=datetime.timezone.utc)
            local = now.astimezone(zoneinfo.ZoneInfo(f.past_window_zone(now)))
            self.assertTrue(6 <= local.hour <= 21, (hour, local))

    def fake(self, latest, version_after=None, sums_ok=True, local_hour='09', planned_ok=True):
        old, new = '2026.11.27.0', '2026.11.27.3'
        body = b'pb'
        digest = hashlib.sha256(body).hexdigest() if sums_ok else '0' * 64

        def fetch(url, timeout=0):
            if url.endswith('/releases/latest'):
                return json.dumps({'tag_name': latest}).encode()
            if url.endswith('SHA256SUMS'):
                return ('# paperboat-release %s\n%s  pb-linux-amd64\n' % (old, digest)).encode()
            return body
        versions = iter([old, version_after or old] + [version_after or old] * 100)

        def at(prefix, command, **kw):
            if command == 'date +%H':
                return mock.Mock(stdout=local_hour + '\n')
            if '--version' in command:
                return mock.Mock(stdout=json.dumps({'ok': True, 'data': {'version': next(versions)}}))
            if 'update check' in command:
                return mock.Mock(stdout=json.dumps({'ok': True, 'data': {'latest_version': new, 'update_available': True}}))
            planned = (datetime.datetime.now(zoneinfo.ZoneInfo('Asia/Kolkata')) + datetime.timedelta(days=1)).replace(hour=4, minute=0, second=0, microsecond=0)
            return mock.Mock(stdout=json.dumps({'ok': True, 'data': {'next_maintenance_at': (planned if planned_ok else planned - datetime.timedelta(hours=5, minutes=30)).isoformat()}}))
        m = mock.Mock()
        m.machine.return_value = ('au4', 'machine_x', 'ssh h docker exec au4')
        m.at.side_effect = at
        env = {'SMOKE_RELEASE_OLD': old, 'SMOKE_RELEASE_NEW': new, 'SMOKE_AU4_OBSERVE': '120', 'SMOKE_AU4_HOST': 'lp9'}
        patches = [mock.patch.object(f, 'fetch', fetch), mock.patch.dict(os.environ, env),
                   mock.patch.object(f, 'run', lambda *a, **k: mock.Mock(stdout='x86_64\n')),
                   mock.patch.object(f.time, 'sleep', lambda s: None), mock.patch.object(f, 'past_window_zone', lambda: 'Asia/Kolkata'),
                   mock.patch.object(f.time, 'monotonic', mock.Mock(side_effect=range(0, 100000, 60)))]
        for patch in patches:
            patch.start()
            self.addCleanup(patch.stop)
        return m

    def test_passes_when_the_fresh_install_keeps_a(self):
        m = self.fake('2026.11.27.3')
        ok, detail = f.fresh_install_window(m)
        self.assertTrue(ok, detail)
        self.assertEqual(m.machine.call_args.kwargs['host'], 'lp9')
        self.assertTrue(m.machine.call_args.kwargs['zone'])

    def test_fails_when_b_installs_early(self):
        ok, detail = f.fresh_install_window(self.fake('2026.11.27.3', version_after='2026.11.27.3'))
        self.assertFalse(ok)
        self.assertIn('outside the window', detail)

    def test_refuses_before_b_is_latest(self):
        m = self.fake('2026.11.27.0')
        ok, detail = f.fresh_install_window(m)
        self.assertFalse(ok)
        self.assertIn('precondition', detail)
        m.machine.assert_not_called()

    def test_refuses_an_asset_that_does_not_match_its_sums(self):
        ok, detail = f.fresh_install_window(self.fake('2026.11.27.3', sums_ok=False))
        self.assertFalse(ok)
        self.assertIn('SHA256SUMS', detail)

    def test_fails_when_the_zone_did_not_take(self):
        ok, detail = f.fresh_install_window(self.fake('2026.11.27.3', local_hour='02'))
        self.assertFalse(ok)
        self.assertIn('local time', detail)


    def test_fails_when_pb_plans_the_window_in_another_zone(self):
        ok, detail = f.fresh_install_window(self.fake('2026.11.27.3', planned_ok=False))
        self.assertFalse(ok)
        self.assertIn('did not take', detail)


if __name__ == '__main__':
    unittest.main()
