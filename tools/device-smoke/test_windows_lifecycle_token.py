import unittest

from unittest import mock
import windows_lifecycle


class EnrollmentTokenTest(unittest.TestCase):
    def test_reads_the_token_of_the_current_windows_command(self):
        command = "$env:PB_TOKEN='0123456789ABCDEFGHIJKLMNOP'; $env:PB_NAME='rx38q-w'; irm https://get.example.test | iex"
        self.assertEqual(windows_lifecycle.enrollment_token(command), '0123456789ABCDEFGHIJKLMNOP')

    def test_a_command_without_a_token_is_refused(self):
        with self.assertRaisesRegex(RuntimeError, 'no PB_TOKEN'):
            windows_lifecycle.enrollment_token("irm https://get.example.test | iex")

    def test_reads_the_token_from_the_machine_add_json_shape(self):
        output = ('{"schema_version":1,"ok":true,"data":{"expires_at":"2026-11-21T12:00:00Z",'
                  '"linux_macos_command":"curl -fsSL https://get.example.test | PB_TOKEN=\'TESTTOKEN123\' sh",'
                  '"windows_command":"$env:PB_TOKEN=\'TESTTOKEN123\'; irm https://get.example.test | iex"}}')
        self.assertEqual(windows_lifecycle.enrollment_token_from_add(output), 'TESTTOKEN123')

    def test_garbage_output_fails_loudly_without_echoing_it(self):
        garbage = 'not json PB_TOKEN=\'TESTTOKEN123\''
        with self.assertRaisesRegex(RuntimeError, 'no windows_command') as raised:
            windows_lifecycle.enrollment_token_from_add(garbage)
        self.assertNotIn('TESTTOKEN123', str(raised.exception))

    def test_json_without_windows_command_fails_loudly(self):
        with self.assertRaisesRegex(RuntimeError, 'no windows_command'):
            windows_lifecycle.enrollment_token_from_add('{"ok":true,"data":{"linux_macos_command":"x"}}')

    def test_failed_mint_says_why_without_a_token(self):
        out = "Error: rate limited, support_700b85e2-cb11 $env:PB_TOKEN='0123456789ABCDEFGHIJKLMNOP'"
        with mock.patch.object(windows_lifecycle, 'linux', return_value=(1, out)):
            with self.assertRaises(RuntimeError) as caught:
                windows_lifecycle.mint_token('linux', 'win')
        self.assertIn('exit 1): Error: rate limited, support_700b85e2-cb11', str(caught.exception))
        self.assertNotIn('0123456789ABCDEF', str(caught.exception))

if __name__ == '__main__':
    unittest.main()
