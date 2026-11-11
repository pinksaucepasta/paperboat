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


class CheckTest(unittest.TestCase):
    def test_install_and_pair_are_refused_in_the_desktop_task(self):
        for command in ('install --json', 'pair --name x', 'uninstall --json', 'reset'):
            with self.assertRaises(SystemExit) as raised:
                flash.check(FakeDevice(), 'x', 'pb.exe', command)
            self.assertIn('ssh', str(raised.exception))

    def test_a_failed_command_is_not_a_pass(self):
        results = iter([{'flashes': [1]}, {'flashes': [], 'command': 'pb.exe service stop', 'exit': 1, 'output': 'boom'}])
        original = flash.run_in_session
        flash.run_in_session = lambda *a, **k: next(results)
        try:
            with self.assertRaises(SystemExit) as raised:
                flash.check(FakeDevice(), 'x', 'pb.exe', 'service stop')
        finally:
            flash.run_in_session = original
        self.assertIn('exited 1', str(raised.exception))


if __name__ == '__main__':
    unittest.main()
