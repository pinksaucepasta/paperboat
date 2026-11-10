import contextlib
import io
import json
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import perf_dataplane as perf


class LocalCLIBudget(unittest.TestCase):
    def test_windows_latency_uses_the_device_samples(self):
        device = SimpleNamespace(ps=lambda *args, **kwargs: (0, '[9,1,7,3,5,11,13]'))
        with patch.object(perf, 'PB', SimpleNamespace(device=device)):
            self.assertEqual(7, perf.local_command_ms('--version'))

    def test_incomplete_or_invalid_device_timings_cannot_pass(self):
        for samples in ([1], [True] * 7, [-1] * 7, [float('nan')] * 7):
            with self.subTest(samples=samples):
                device = SimpleNamespace(ps=lambda *args, **kwargs: (0, json.dumps(samples)))
                with patch.object(perf, 'PB', SimpleNamespace(device=device)):
                    with self.assertRaisesRegex(RuntimeError, 'every local pb timing sample'):
                        perf.local_command_ms('status')

    def test_cli_only_has_no_peer_and_fails_when_a_local_budget_is_exceeded(self):
        for too_slow in (False, True):
            with self.subTest(too_slow=too_slow):
                start = perf.BUDGETS['cli']['cli_start_ms'][1] - 1
                status = perf.BUDGETS['cli']['status_ms'][1] + (1 if too_slow else -1)
                output = io.StringIO()
                with patch('sys.argv', ['perf_dataplane.py', '--cli-only']), \
                        patch.object(perf, 'registration', return_value=1), \
                        patch.object(perf, 'local_command_ms', side_effect=[start, status]), \
                        patch.object(perf, 'selected_path', side_effect=AssertionError('no peer may be used')), \
                        contextlib.redirect_stdout(output):
                    self.assertEqual(1 if too_slow else 0, perf.main())
                receipt = json.loads(output.getvalue())
                self.assertEqual('FAIL' if too_slow else 'PASS', receipt['status'])
                self.assertEqual(['status_ms'] if too_slow else [], receipt['budget_failures'])
                self.assertTrue(receipt['enrollment_unchanged'])


if __name__ == '__main__':
    unittest.main()
