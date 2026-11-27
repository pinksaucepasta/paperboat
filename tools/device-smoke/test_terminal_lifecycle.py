import os
from pathlib import Path
import shutil
import sys
import time
import unittest
from unittest import mock

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

            def wait_windows_prompt(self):
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

    def test_windows_phases_type_nothing_before_the_prompt_is_shown(self):
        calls = []

        class Stop(Exception):
            pass

        class Fake:
            def wait_windows_prompt(self):
                calls.append('prompt')

            def expect(self, *args, **kwargs):
                calls.append('expect')
                raise Stop

            def send(self, data):
                calls.append('send')

        with self.assertRaises(Stop):
            terminal_lifecycle.windows_phases(Fake(), 'main', 'off')
        self.assertEqual(['prompt', 'send', 'expect'], calls)

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


# A Windows target's terminal as ConPTY paints it: the banner, a pause, then the prompt. Like cmd.exe on the real
# machine it echoes a line typed before the prompt and never runs it.
EARLY_TYPING_CONSOLE = r"""
import os, select, sys, time, tty
tty.setraw(0)
os.write(1, b'\x1b]0;C:\\WINDOWS\\system32\\cmd.exe\x07Microsoft Windows [Version 10.0]\r\n(c) Microsoft Corporation. All rights reserved.\r\n')
ready = time.monotonic() + 1.0
while time.monotonic() < ready:
    if select.select([0], [], [], 0.05)[0]:
        os.write(1, os.read(0, 4096))  # echoed and discarded
os.write(1, b'\x1b[4;1H\x1b[?25hC:\\Users\\someone>')
line = b''
while True:
    chunk = os.read(0, 4096)
    if not chunk:
        break
    os.write(1, chunk)
    line += chunk
    while b'\r' in line:
        typed, line = line.split(b'\r', 1)
        os.write(1, b'\r\nRAN<' + typed + b'>\r\n')
"""


class WindowsPrompt(unittest.TestCase):
    def console(self, script=EARLY_TYPING_CONSOLE):
        terminal = Terminal([sys.executable, '-c', script])
        self.addCleanup(terminal.close)
        return terminal

    def test_a_line_typed_after_the_prompt_wait_runs(self):
        terminal = self.console()
        terminal.wait_windows_prompt(timeout=30)
        terminal.send('say hello\r')
        terminal.expect('RAN<say hello>', timeout=10)

    def test_a_line_typed_before_the_prompt_is_only_echoed(self):  # what the fixture stands for: WA6
        terminal = self.console()
        terminal.wait_raw()
        terminal.send('say early\r')
        terminal.expect('say early', timeout=10)
        with self.assertRaisesRegex(RuntimeError, 'did not produce'):
            terminal.expect('RAN<say early>', timeout=3)

    def test_no_prompt_is_an_error_and_a_banner_or_window_title_is_not_a_prompt(self):
        script = EARLY_TYPING_CONSOLE.replace('ready = time.monotonic() + 1.0', 'ready = time.monotonic() + 60')
        terminal = self.console(script)
        with self.assertRaisesRegex(RuntimeError, 'did not show its prompt within 2 s'):
            terminal.wait_windows_prompt(timeout=2)

    def test_prompt_forms(self):
        for shown in (b'C:\\Users\\runneradmin>', b'PS C:\\Windows\\system32> ', b'C:\\>', b'D:\\a b\\c(1)>'):
            self.assertTrue(terminal_lifecycle.WINDOWS_PROMPT.search(shown), shown)
        for other in (b'(c) Microsoft Corporation. All rights reserved.\r\n', b'C:\\Users\\x\r\n>', b'$ ', b'https://host/a>'):
            self.assertFalse(terminal_lifecycle.WINDOWS_PROMPT.search(other), other)

    def test_every_first_keystroke_into_a_windows_terminal_waits_for_the_prompt(self):
        here = Path(__file__).parent
        for name in ('session_limit.py', 'doctor_lifecycle.py', 'terminal_matrix.py', 'session_lifecycle.py',
                     'terminal_lifecycle.py', 'session_idle.py', 'recordings_lifecycle.py', 'update_continuity.py',
                     'update_survival.py'):
            with self.subTest(name):
                self.assertIn('.wait_windows_prompt(', (here / name).read_text())


class SessionLimitOpensAWindowsTerminal(unittest.TestCase):
    def test_new_types_its_marker_only_after_the_prompt(self):
        import session_limit
        calls = []

        class Fake:
            def __init__(self, *args, **kwargs):
                pass

            def wait_windows_prompt(self):
                calls.append('prompt')

            def send(self, data):
                calls.append('send')

            def expect(self, token, timeout=45):
                calls.append('expect')

            def close(self):
                pass

        with mock.patch.object(session_limit, 'Terminal', Fake), \
                mock.patch.object(session_limit.windows_target, 'is_windows', lambda *a: True):
            self.assertEqual((0, ''), session_limit.new('smoke-limit-x', 'windows-target'))
        self.assertEqual(['prompt', 'send', 'expect'], calls)


if __name__ == '__main__':
    unittest.main()
