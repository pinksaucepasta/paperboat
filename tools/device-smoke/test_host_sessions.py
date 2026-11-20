"""The preflight warns on a test host with many login sessions and fails on a wedged logind."""
import io
import subprocess
import unittest
from contextlib import redirect_stderr
from unittest.mock import MagicMock

import host_sessions


def runner(counts):
    def run(command, **kwargs):
        host = command[-2]
        value = counts[host]
        if isinstance(value, Exception):
            raise value
        return MagicMock(returncode=0 if value is not None else 1, stdout='%s\n' % value)
    return run


class HostSessions(unittest.TestCase):
    def test_thresholds(self):
        self.assertEqual(host_sessions.verdict('h', 4)[0], 'ok')
        self.assertEqual(host_sessions.verdict('h', 50)[0], 'ok')
        self.assertEqual(host_sessions.verdict('h', 51)[0], 'warn')
        self.assertEqual(host_sessions.verdict('h', 500)[0], 'warn')
        self.assertEqual(host_sessions.verdict('h', 501)[0], 'fail')

    def test_a_wedged_host_fails_with_the_restart_hint(self):
        status, message = host_sessions.verdict('pbvm3', 1452)
        self.assertEqual(status, 'fail')
        self.assertIn('ssh pbvm3 sudo systemctl restart systemd-logind', message)

    def test_main_exit_status_and_stderr(self):
        err = io.StringIO()
        with redirect_stderr(err):
            self.assertEqual(host_sessions.main(['a', 'b'], runner({'a': 3, 'b': 80})), 0)  # a warning does not fail
            self.assertEqual(host_sessions.main(['a', 'c'], runner({'a': 3, 'c': 1452})), 1)
        self.assertIn('preflight warning: b holds 80', err.getvalue())
        self.assertIn('preflight FAIL: c holds 1452', err.getvalue())

    def test_a_host_that_cannot_be_counted_warns_but_does_not_fail(self):
        err = io.StringIO()
        with redirect_stderr(err):
            self.assertEqual(host_sessions.main(['x', 'y', 'z'], runner({'x': None, 'y': subprocess.TimeoutExpired('ssh', 60), 'z': OSError('no ssh')})), 0)
        self.assertEqual(err.getvalue().count('could not be counted'), 3)


if __name__ == '__main__':
    unittest.main()
