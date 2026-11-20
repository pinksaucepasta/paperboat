import os
import shutil
import sys
import time
import unittest

import terminal_lifecycle
from terminal_lifecycle import Terminal, applications, windows_phases


@unittest.skipUnless(all(shutil.which(name) for name in ['bash', 'vim', 'less']),
                     'real interactive shell, Vim and less are required')
class FullscreenApplications(unittest.TestCase):
    def test_app_exit_waits_for_the_shell_before_sending_another_command(self):
        terminal = Terminal([shutil.which('bash'), '--noprofile', '--norc', '-i'])
        try:
            terminal.expect(terminal.marker('READY', 'stty -echo; '), timeout=10)
            prompt = applications(terminal)
            terminal.expect(terminal.marker('AFTER_APPLICATIONS'))
            # Ctrl-D sent before the shell is back at its prompt is lost by the tty line discipline (Terminal.settle):
            # wait for the prompt itself, however long a loaded host takes, then for the exit with a generous bound.
            terminal.expect(prompt, timeout=120)
            terminal.send(b'\x04')
            self.assertEqual(0, terminal.wait_exit(timeout=120))
        finally:
            terminal.close()


class WindowsInterrupt(unittest.TestCase):
    def test_recovery_input_is_sent_after_cmd_returns(self):
        class Console:
            awaiting_prompt = False

            def wait_raw(self):
                pass

            def send(self, data):
                if self.awaiting_prompt:
                    raise AssertionError('typed while the foreground process was handling Ctrl-C')
                if data == b'\x03':
                    self.awaiting_prompt = True

            def expect(self, token, timeout=45):
                if token == 'PB_CTRL_C_PROMPT>':
                    self.awaiting_prompt = False

            def resize(self, rows, columns):
                pass

            def settle(self):
                pass

            def wait_exit(self):
                return 0

        windows_phases(Console(), 'main', 'off')


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

    def test_wait_raw_returns_once_the_program_leaves_line_mode_and_fails_if_it_never_does(self):
        # A program started on the pty is in line mode (echo on, CR becomes LF) until it sets raw, as ssh -tt does late.
        late = Terminal([sys.executable, '-c', "import time,tty; time.sleep(0.4); tty.setraw(0); time.sleep(5)"])
        try:
            started = time.monotonic()
            late.wait_raw(timeout=10)
            self.assertGreaterEqual(time.monotonic() - started, 0.3)
        finally:
            late.close()
        never = Terminal([sys.executable, '-c', "import time; time.sleep(5)"])
        try:
            with self.assertRaisesRegex(RuntimeError, 'raw mode'):
                never.wait_raw(timeout=0.5)
        finally:
            never.close()

    def test_windows_phases_type_nothing_before_the_pty_is_raw(self):
        calls = []

        class Stop(Exception):
            pass

        class Fake:
            def wait_raw(self):
                calls.append('raw')

            def expect(self, *args, **kwargs):
                calls.append('expect')
                raise Stop

            def send(self, data):
                calls.append('send')

        with self.assertRaises(Stop):
            terminal_lifecycle.windows_phases(Fake(), 'main', 'off')
        self.assertEqual(['raw', 'send', 'expect'], calls)

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
