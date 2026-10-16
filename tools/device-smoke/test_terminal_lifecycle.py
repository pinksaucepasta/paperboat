import os
import shutil
import sys
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


class TerminalCapabilities(unittest.TestCase):
    def test_pty_answers_capability_queries_before_the_application_starts(self):
        expected = b'\x1b]11;rgb:0000/0000/0000\x07\x1b[1;1R'
        for ending in [b'\x07', b'\x1b\\']:
            with self.subTest(ending=ending):
                request = b'\x1b]11;?' + ending + b'\x1b[6n'
                child = ("import os,tty\ntty.setraw(0)\n"
                         "os.write(1," + repr(request) + ")\n"
                         "expected=" + repr(expected) + "\nresponse=b''\n"
                         "while len(response)<len(expected):response+=os.read(0,len(expected)-len(response))\n"
                         "if response!=expected:raise SystemExit(1)\n"
                         "os.write(1,b'PB_QUERY_OK')\n")
                terminal = Terminal([sys.executable, '-c', child])
                try:
                    terminal.expect('PB_QUERY_OK', timeout=2)
                    self.assertEqual(0, terminal.wait_exit())
                finally:
                    terminal.close()

    @unittest.skipUnless(hasattr(os, 'waitid'), 'non-reaping child-exit observation is required')
    def test_exit_receipt_keeps_output_after_the_child_has_already_exited(self):
        child = "import os; os.write(1,b'PB_EXIT_DIAGNOSTIC'); raise SystemExit(23)"
        terminal = Terminal([sys.executable, '-c', child])
        try:
            os.waitid(os.P_PID, terminal.pid, os.WEXITED | os.WNOWAIT)
            self.assertEqual(23, terminal.wait_exit())
            self.assertIn(b'PB_EXIT_DIAGNOSTIC', terminal.output)
        finally:
            terminal.close()


if __name__ == '__main__':
    unittest.main()
