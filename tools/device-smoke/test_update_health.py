import unittest
from unittest import mock

import update_health as h

A = 'a' * 64
PARTS = {'cli': A, 'paperboat-hostd-u1000-g.service': A, 'paperboat-updated-u1000.service': A,
         'paperboat-carrier-u1000-g.service': A, 'paperboat-machineguard.service': A, 'paperboatd': A}
HEALTHY = {'ok': True, 'data': {'overall': 'healthy', 'checks': [{'code': 'machine_guard_version', 'status': 'pass'}]}}
STATUS = {'cli_version': 'B', 'runtime_version': 'B'}


class UpdateHealth(unittest.TestCase):
    def test_healthy_with_every_component_on_the_installed_pb(self):
        self.assertEqual(h.problems('B', HEALTHY, STATUS, 'B', PARTS), [])

    def test_each_way_to_be_left_behind_is_named(self):
        guard_warn = {'data': {'overall': 'degraded', 'checks': [{'code': 'machine_guard_version', 'status': 'warning', 'summary': 'guard runs A'}]}}
        cases = [
            (guard_warn, STATUS, 'B', PARTS, 'machine_guard_version warning: guard runs A'),
            ({'data': {'overall': 'healthy', 'checks': []}}, STATUS, 'B', PARTS, 'machine_guard_version missing'),
            (HEALTHY, dict(STATUS, runtime_version='A'), 'B', PARTS, 'runtime_version A'),
            (HEALTHY, STATUS, 'A', PARTS, 'pb --version A'),
            (HEALTHY, STATUS, 'B', dict(PARTS, **{'paperboat-carrier-u1000-g.service': 'c' * 64}), 'not on the installed pb'),
            (HEALTHY, STATUS, 'B', {k: v for k, v in PARTS.items() if 'hostd' not in k}, 'no running hostd component'),
            (guard_warn, STATUS, 'B', PARTS, 'doctor degraded'),
        ]
        for report, status, version, parts, want in cases:
            self.assertTrue(any(want in w for w in h.problems('B', report, status, version, parts)), want)

    def test_parses_the_component_listing_and_never_drops_an_unread_one(self):
        out = 'cli %s\npaperboat-hostd-u1-g.service %s\ncom.pinksaucepasta.paperboat.carrier.u501.g STALE\nguard\nshort 12ab\n' % (A, A)
        self.assertEqual(h.components(lambda script: out), {'cli': A, 'paperboat-hostd-u1-g.service': A,
                         'com.pinksaucepasta.paperboat.carrier.u501.g': 'STALE', 'guard': 'MISSING', 'short': 'MISSING'})

    def test_a_stale_or_unreadable_component_fails(self):
        for value in ('STALE', 'MISSING'):
            wrong = h.problems('B', HEALTHY, STATUS, 'B', dict(PARTS, **{'paperboat-machineguard.service': value}))
            self.assertTrue(any('paperboat-machineguard.service ' + value in w for w in wrong), wrong)

    def test_settled_waits_for_the_activation_then_reports_the_doctor_output(self):
        clock = mock.Mock(monotonic=mock.Mock(side_effect=[0, 10, 20, 9999]), sleep=lambda s: None)
        versions = iter(['A', 'B'])
        receipt = h.settled('B', 100, doctor=lambda: HEALTHY, status=lambda: STATUS, version=lambda: next(versions),
                            run_shell=lambda s: '\n'.join('%s %s' % kv for kv in PARTS.items()), clock=clock)
        self.assertEqual(receipt['doctor'], 'healthy')
        stuck = mock.Mock(monotonic=mock.Mock(side_effect=[0, 9999]), sleep=lambda s: None)
        with self.assertRaisesRegex(AssertionError, 'pb --version A.*receipt.*"doctor"'):
            h.settled('B', 100, doctor=lambda: HEALTHY, status=lambda: STATUS, version=lambda: 'A',
                      run_shell=lambda s: '\n'.join('%s %s' % kv for kv in PARTS.items()), clock=stuck)


if __name__ == '__main__':
    unittest.main()
