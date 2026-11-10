import unittest
from unittest import mock

import terminal_matrix as matrix


class FakeClient(matrix.Client):
    """A client whose terminal answers the readiness marker only after `lose` Enter presses were lost."""

    def __init__(self, lose, closes=False):
        self.sent, self.lose, self.closes, self.waits = [], lose, closes, 0

    def send(self, data):
        self.sent.append(data)

    def expect(self, token, timeout=45):
        self.waits += 1
        if self.closes:
            raise RuntimeError('terminal closed before the expected fixture output')
        if len(self.sent) - 1 < self.lose:
            raise RuntimeError('terminal did not produce the expected fixture output')
        return b''


class Ready(unittest.TestCase):
    def test_a_windows_target_presses_enter_again_until_the_marker_answers(self):
        with mock.patch.object(matrix, 'win', return_value=True):
            client = FakeClient(lose=2)
            client.ready(timeout=60)
        self.assertEqual(['\r', '\r'], client.sent[1:])  # the marker line once, then only Enter, never the line again
        self.assertEqual(3, client.waits)

    def test_a_closed_terminal_is_not_retried(self):
        with mock.patch.object(matrix, 'win', return_value=True):
            client = FakeClient(lose=0, closes=True)
            with self.assertRaises(RuntimeError):
                client.ready(timeout=60)
        self.assertEqual(1, client.waits)

    def test_a_silent_terminal_fails_at_the_deadline(self):
        with mock.patch.object(matrix, 'win', return_value=True):
            client = FakeClient(lose=10 ** 6)
            with self.assertRaises(RuntimeError):
                client.ready(timeout=0)
        self.assertEqual(1, client.waits)


if __name__ == '__main__':
    unittest.main()
