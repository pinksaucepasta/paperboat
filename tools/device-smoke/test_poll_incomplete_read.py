"""A poll that meets a response cut mid-body (the edge ends a stopped preview's in-flight request) retries; it is not the assertion."""
import http.client
import unittest
from unittest.mock import MagicMock, patch

import preview_survival
import tunnel_reboot
import update_survival


def cut_response(*_args, **_kwargs):
    response = MagicMock()
    response.status = 200
    response.read.side_effect = http.client.IncompleteRead(b'', 58)
    response.__enter__.return_value = response
    return response


class PollIncompleteRead(unittest.TestCase):
    def test_preview_survival_body_treats_a_cut_response_as_not_serving(self):
        with patch('urllib.request.urlopen', cut_response):
            self.assertEqual(preview_survival.body('https://x.example.test'), '')

    def test_update_survival_body_treats_a_cut_response_as_not_serving(self):
        with patch('urllib.request.urlopen', cut_response):
            self.assertEqual(update_survival.body('https://x.example.test'), '')

    def test_tunnel_reboot_fetch_reports_a_cut_response_as_status_zero(self):
        with patch('urllib.request.urlopen', cut_response):
            status, _ = tunnel_reboot.fetch('https://x.example.test')
        self.assertEqual(status, 0)


if __name__ == '__main__':
    unittest.main()
