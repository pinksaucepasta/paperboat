"""session_lifecycle types the target's own shell syntax: bash for Unix, cmd.exe + PowerShell for a Windows target."""
import unittest

import session_lifecycle


class Typed:
    def __init__(self):
        self.sent, self.expected, self.timeouts = [], [], []

    def send(self, data):
        self.sent.append(data)

    def expect(self, text, timeout=None):
        self.expected.append(text)
        self.timeouts.append(timeout)

    def marker(self, label, prefix=''):
        self.send(prefix + "printf '\\nPB_" + label + "_%s\\n' 'x'\r")
        return 'PB_' + label + '_x'


class SessionLifecycleShell(unittest.TestCase):
    def run_all(self, windows):
        terminal = Typed()
        session_lifecycle.ready(terminal, windows)
        session_lifecycle.keep_state(terminal, windows, 'abc123')
        session_lifecycle.show_state(terminal, windows, 'abc123')
        return terminal

    def test_a_windows_target_is_typed_cmd_lines_with_no_bash_and_no_powershell(self):
        terminal = self.run_all(True)
        typed = ''.join(terminal.sent)
        for foreign in ('stty', 'printf', '$PB_DURABLE', "PB_DURABLE='", 'powershell'):
            self.assertNotIn(foreign, typed)
        self.assertIn('set PB_DURABLE=abc123\r', typed)
        # The caret keeps the echoed command line from matching the marker the line prints; %VAR% expands when it runs.
        self.assertIn('echo PB_^DURABLE_%PB_DURABLE%\r', typed)
        self.assertIn('echo PB_^REATTACHED_%PB_DURABLE%\r', typed)
        self.assertRegex(typed, r'echo PB_\^READY_[0-9a-f]{32}\r')
        self.assertNotIn(b'\x1b[?2004h', terminal.expected)  # cmd.exe does not enable bracketed paste
        self.assertEqual(['PB_DURABLE_abc123', 'PB_REATTACHED_abc123'], [e for e in terminal.expected if e.endswith('abc123')])

    def test_a_windows_target_is_given_60_seconds_to_print_ready(self):
        terminal = self.run_all(True)
        self.assertEqual(60, terminal.timeouts[terminal.expected.index(next(e for e in terminal.expected if e.startswith('PB_READY_')))])

    def test_a_unix_target_is_typed_as_before(self):
        terminal = self.run_all(False)
        typed = ''.join(terminal.sent)
        self.assertIn('stty -echo; ', typed)
        self.assertIn("PB_DURABLE='abc123'; printf", typed)
        self.assertIn('printf \'\\nPB_REATTACHED_%s\\n\' "$PB_DURABLE"', typed)
        self.assertEqual(b'\x1b[?2004h', terminal.expected[0])


if __name__ == '__main__':
    unittest.main()
