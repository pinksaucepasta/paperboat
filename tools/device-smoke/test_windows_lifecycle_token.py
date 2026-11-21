import unittest

import windows_lifecycle


class EnrollmentTokenTest(unittest.TestCase):
    def test_reads_the_token_of_the_current_windows_command(self):
        command = "$env:PB_TOKEN='0123456789ABCDEFGHIJKLMNOP'; $env:PB_NAME='rx38q-w'; irm https://get.example.test | iex"
        self.assertEqual(windows_lifecycle.enrollment_token(command), '0123456789ABCDEFGHIJKLMNOP')

    def test_a_command_without_a_token_is_refused(self):
        with self.assertRaisesRegex(RuntimeError, 'no PB_TOKEN'):
            windows_lifecycle.enrollment_token("irm https://get.example.test | iex")


if __name__ == '__main__':
    unittest.main()
