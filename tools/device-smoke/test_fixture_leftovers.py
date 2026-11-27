"""A fixture machine whose revoke fails is reported as left behind, never dropped."""
import subprocess
import unittest
from unittest import mock

import fixture_checks


class FixtureLeftovers(unittest.TestCase):
    def run_down(self, code):
        made = subprocess.CompletedProcess([], 0, '{"machine_id": "machine_1", "prefix": "ssh h docker exec n"}', '')
        down = subprocess.CompletedProcess([], code, '', 'revoke of machine_1 failed' if code else '')
        with mock.patch.dict(fixture_checks.ENV, {'SMOKE_CONSOLE_ORIGIN': 'https://console.example.test'}), \
                mock.patch.object(fixture_checks, 'path', return_value='/x'), \
                mock.patch.object(fixture_checks, 'run', side_effect=[made, down]):
            with fixture_checks.Machines() as machines:
                machines.machine('peer', state='/state')
        return machines.leftovers

    def test_a_failed_revoke_is_a_leftover(self):
        self.assertIn('NOT revoked: revoke of machine_1 failed', self.run_down(1)[0])

    def test_a_clean_revoke_leaves_nothing(self):
        self.assertEqual([], self.run_down(0))


if __name__ == '__main__':
    unittest.main()
