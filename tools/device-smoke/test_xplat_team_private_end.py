import unittest

from xplat_team_private_end import closure_seconds


class ClosureTimingTest(unittest.TestCase):
    def test_requires_a_flow_with_traffic_and_actual_closure_within_bound(self):
        closed = {'ended': True, 'echoes': 2}
        self.assertEqual(closure_seconds(closed, 100, 130), 30)
        for state, observed in [({'alive': True, 'echoes': 20}, 102),
                                ({'ended': True, 'echoes': 0}, 102),
                                (closed, 130.01), (closed, 99), (closed, float('nan'))]:
            with self.subTest(state=state, observed=observed), self.assertRaises(AssertionError):
                closure_seconds(state, 100, observed)


if __name__ == '__main__': unittest.main()
