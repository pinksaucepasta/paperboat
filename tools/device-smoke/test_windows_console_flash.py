import unittest

import windows_console_flash as flash


class FakeDevice:
    def __init__(self):
        self.calls = []

    def env(self, name):
        self.calls.append('env')
        return 'C:\\Temp'


class ConsoleGuardTest(unittest.TestCase):
    def test_console_exe_outside_control_is_refused_before_touching_the_device(self):
        for exe in ('cmd.exe', 'C:\\Windows\\System32\\CMD.EXE', 'powershell.exe'):
            device = FakeDevice()
            with self.assertRaises(SystemExit) as raised:
                flash.run_in_session(device, 'x', exe, '/c ver', 5)
            self.assertIn('visible console', str(raised.exception))
            self.assertEqual(device.calls, [], exe)


if __name__ == '__main__':
    unittest.main()
