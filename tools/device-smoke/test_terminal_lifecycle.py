import shutil
import unittest

from terminal_lifecycle import Terminal, applications


@unittest.skipUnless(all(shutil.which(name) for name in ['bash', 'vim', 'less']),
                     'real interactive shell, Vim and less are required')
class FullscreenApplications(unittest.TestCase):
    def test_app_exit_waits_for_the_shell_before_sending_another_command(self):
        terminal = Terminal([shutil.which('bash'), '--noprofile', '--norc', '-i'])
        try:
            terminal.expect(terminal.marker('READY', 'stty -echo; '), timeout=10)
            applications(terminal)
            terminal.expect(terminal.marker('AFTER_APPLICATIONS'))
            terminal.send(b'\x04')
            self.assertEqual(0, terminal.wait_exit())
        finally:
            terminal.close()


if __name__ == '__main__':
    unittest.main()
