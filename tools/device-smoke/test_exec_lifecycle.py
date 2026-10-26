import unittest

from exec_lifecycle import without_env_notice


class EnvNoticeTest(unittest.TestCase):
    def test_only_the_cli_env_notice_is_dropped(self):
        notice = b'Paperboat ENV not applied: delivery to this machine is pending. Run `pb env vault unlock`.\n'
        self.assertEqual(without_env_notice(notice + 'PB_STDERR_λ船\n'.encode()), 'PB_STDERR_λ船\n'.encode())
        self.assertEqual(without_env_notice(notice), b'')
        self.assertEqual(without_env_notice(b'x Paperboat ENV not applied: y\n'), b'x Paperboat ENV not applied: y\n')


if __name__ == '__main__':
    unittest.main()
