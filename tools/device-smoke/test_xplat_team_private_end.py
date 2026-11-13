import unittest

from xplat_team_private_end import closure_seconds, raw_denied


class ClosureTimingTest(unittest.TestCase):
    def test_requires_a_flow_with_traffic_and_actual_closure_within_bound(self):
        closed = {'ended': True, 'echoes': 2, 'flows': {k: {'echoes': 2} for k in ('access', 'raw')}}
        self.assertEqual(closure_seconds(closed, 100, 130), 30)
        for state, observed in [({'alive': True, 'echoes': 20}, 102),
                                ({'ended': True, 'echoes': 0}, 102),
                                ({'ended': True, 'echoes': 2, 'flows': {'access': {'echoes': 2}}}, 102),
                                (closed, 130.01), (closed, 99), (closed, float('nan'))]:
            with self.subTest(state=state, observed=observed), self.assertRaises(AssertionError):
                closure_seconds(state, 100, observed)


class FreshRawDenialTest(unittest.TestCase):
    def test_only_mac_connect_drop_counts_as_denial(self):
        for platform in ('linux', 'darwin', 'windows'):
            with self.subTest(platform=platform):
                self.assertEqual(raw_denied(platform, {'phase': 'connect', 'outcome': 'timeout'}), platform == 'darwin')
                denied = {'phase': 'connect', 'outcome': 'permission', 'winerror': 10013}
                self.assertEqual(raw_denied(platform, denied), platform == 'windows')
                self.assertFalse(raw_denied(platform, {**denied, 'phase': 'receive'}))
                self.assertFalse(raw_denied(platform, {'phase': 'receive', 'outcome': 'timeout'}))
                self.assertFalse(raw_denied(platform, {'phase': 'receive', 'outcome': 'data'}))
                self.assertTrue(raw_denied(platform, {'phase': 'receive', 'outcome': 'EOF'}))


if __name__ == '__main__': unittest.main()
