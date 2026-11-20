"""session_lifecycle types the target's own shell syntax: bash for Unix, cmd.exe + PowerShell for a Windows target."""
import unittest

import session_lifecycle


class Typed:
    def __init__(self):
        self.sent, self.expected = [], []

    def send(self, data):
        self.sent.append(data)

    def expect(self, text, timeout=None):
        self.expected.append(text)

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

    def test_a_windows_target_is_typed_no_bash(self):
        typed = ''.join(self.run_all(True).sent)
        for bashism in ('stty', 'printf', '$PB_DURABLE', "PB_DURABLE='"):
            self.assertNotIn(bashism, typed)
        self.assertIn('set PB_DURABLE=abc123\r', typed)
        self.assertIn('powershell -NoProfile -Command', typed)
        self.assertIn('$env:PB_DURABLE', typed)
        self.assertNotIn(b'\x1b[?2004h', self.run_all(True).expected)  # cmd.exe does not enable bracketed paste
        self.assertEqual(['PB_DURABLE_abc123', 'PB_REATTACHED_abc123'], [e for e in self.run_all(True).expected if e.endswith('abc123')])

    def test_a_unix_target_is_typed_as_before(self):
        terminal = self.run_all(False)
        typed = ''.join(terminal.sent)
        self.assertIn('stty -echo; ', typed)
        self.assertIn("PB_DURABLE='abc123'; printf", typed)
        self.assertIn('printf \'\\nPB_REATTACHED_%s\\n\' "$PB_DURABLE"', typed)
        self.assertEqual(b'\x1b[?2004h', terminal.expected[0])


if __name__ == '__main__':
    unittest.main()
