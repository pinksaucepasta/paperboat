import argparse
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import build_devices  # noqa: E402


class ApiURLTest(unittest.TestCase):
    def test_accepts_the_api_origin(self):
        self.assertEqual(build_devices.api_url(' https://api.example.test/ '), 'https://api.example.test')

    def test_refuses_userinfo_console_and_non_https(self):
        for value in ('https://user:secret@api.example.test', 'https://user@api.example.test',
                      'https://app.example.test', 'http://api.example.test', 'https://api.example.test/v1'):
            with self.assertRaises(argparse.ArgumentTypeError, msg=value):
                build_devices.api_url(value)


if __name__ == '__main__':
    unittest.main()
