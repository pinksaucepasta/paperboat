import hashlib
from pathlib import Path
import sys
import textwrap
import unittest
from unittest.mock import patch

import terminal_lifecycle as tl

# A stand-in for `pb connect` that is slow to take the terminal: the drop is typed before it is in raw mode.
FAKE = textwrap.dedent('''
    import os, sys, termios, time, tty
    time.sleep(0.7)
    tty.setraw(0, termios.TCSANOW)  # as Go's term.MakeRaw: pending input is kept
    data = b''
    while not data.endswith(b'\\x1b[201~'):
        data += os.read(0, 1024)
    os.write(1, b'GOT ' + data[6:-6] + b' END')
    time.sleep(30)
''')


class Drop(unittest.TestCase):
    def terminal(self, script=FAKE):
        terminal = tl.Terminal([sys.executable, '-c', script])
        self.addCleanup(terminal.close)
        return terminal

    def test_the_notice_that_pb_typed_the_local_path_is_found_also_as_conpty_draws_it(self):
        self.assertIn('file transfer unavailable', tl.drop_refused(b'\r\n[paperboat] file transfer unavailable; pasting original path\r\n'))
        self.assertIn('file upload failed', tl.drop_refused(b'[paperboat]\x1b[1Cfile\x1b[1Cupload failed; pasting\r\n\x1b[5;1Horiginal path'))
        self.assertIsNone(tl.drop_refused(b'$ /tmp/paperboat-smoke-drop-1.bin'))

    def test_a_drop_on_the_same_machine_needs_the_typed_path_not_the_local_echo(self):
        name = 'paperboat-smoke-drop-1.bin'
        wrapped = b'[paperboat] file transfer unavailable; pasting original path\r\n$ \x1b[7m/tmp/paperboat-smoke-drop-1.b\x1b[27mi\r\nn'
        self.assertTrue(tl.pasted_local_path(wrapped, name))
        self.assertTrue(tl.pasted_local_path(b'C:\\Users\\a>C:\\Temp\\paperboat-smoke-drop-1.bin\x1b[K', name))  # no notice: ConPTY
        self.assertFalse(tl.pasted_local_path(b'^[[200~/tmp/paperboat-smoke-drop-1.bin^[[201~\r\n$ ', name))
        self.assertFalse(tl.pasted_local_path(b'$ /home/a/Paperboat/Inbox/paperboat-smoke-drop-1.bin', name))
        self.assertFalse(tl.pasted_local_path(b'[paperboat] file upload failed; pasting original path\r\n$ /tmp/paperboat-smoke-drop-1.bin', name))
        with self.assertRaisesRegex(RuntimeError, 'did not type the local path'):
            tl.await_local_paste(self.terminal('import time; time.sleep(30)'), name, bound=0)

    def test_the_drop_is_sent_at_once_and_reaches_a_program_that_is_not_reading_yet(self):
        terminal = self.terminal()
        with patch.object(tl, 'received_hash', side_effect=lambda *a: 'abc' if b'END' in terminal.output else None):
            tl.drop_file(terminal, 'pb', 'peer', Path('/tmp/drop.bin'), 'abc')
        self.assertIn(b'GOT /tmp/drop.bin END', terminal.output)

    def test_a_windows_client_drops_only_after_pb_turns_bracketed_paste_on(self):
        # As a Windows console: input sent before pb asks for bracketed paste loses its brackets (here: all of it).
        console = textwrap.dedent('''
            import os, sys, termios, time, tty, select
            tty.setraw(0, termios.TCSANOW)
            time.sleep(0.7)
            while select.select([0], [], [], 0)[0]:
                os.read(0, 1024)
            os.write(1, b'\\x1b[?2004h')
            data = b''
            while not data.endswith(b'\\x1b[201~'):
                data += os.read(0, 1024)
            os.write(1, b'GOT ' + data[6:-6] + b' END')
            time.sleep(30)
        ''')
        terminal = self.terminal(console)
        tl.await_paste_mode(terminal, bound=20)
        with patch.object(tl, 'received_hash', side_effect=lambda *a: 'abc' if b'END' in terminal.output else None):
            tl.drop_file(terminal, 'pb', 'peer', Path('/tmp/drop.bin'), 'abc')
        self.assertIn(b'GOT /tmp/drop.bin END', terminal.output)
        with self.assertRaisesRegex(RuntimeError, 'expected fixture output'):
            tl.await_paste_mode(self.terminal('import time; time.sleep(30)'), bound=1)

    def test_the_unavailable_notice_fails_even_if_the_file_is_there(self):
        terminal = self.terminal('print("[paperboat] file transfer unavailable; pasting original path", flush=True); import time; time.sleep(30)')
        with self.assertRaisesRegex(RuntimeError, 'file transfer unavailable; pasting original path'):
            tl.await_drop(terminal, 'abc', lambda: 'abc', bound=20)

    def test_another_hash_and_a_file_that_never_arrives_fail(self):
        with self.assertRaisesRegex(RuntimeError, 'another sha256'):
            tl.await_drop(self.terminal(), 'abc', lambda: 'def', bound=20)
        with self.assertRaisesRegex(RuntimeError, 'not on the target within 0 s'):
            tl.await_drop(self.terminal(), 'abc', lambda: None, bound=0)

    def test_the_notice_is_the_reason_when_pb_exits_right_after_it(self):
        terminal = self.terminal('print("[paperboat] file transfer unavailable; pasting original path", flush=True)')
        with self.assertRaisesRegex(RuntimeError, 'file transfer unavailable; pasting original path'):
            tl.await_drop(terminal, 'abc', lambda: None, bound=20)

    def test_a_pb_that_exits_fails(self):
        with self.assertRaisesRegex(RuntimeError, 'terminal closed'):
            tl.await_drop(self.terminal('pass'), 'abc', lambda: None, bound=20)

    def test_the_inbox_query_reads_a_unix_target(self):
        digest = hashlib.sha256(b'x').hexdigest()
        with patch.object(tl.windows_target, 'is_windows', return_value=False):
            with patch.object(tl, 'run_pb_exec', return_value=(b'PB_DROP ' + digest.encode() + b'\n', b'')) as call:
                self.assertEqual(tl.received_hash('pb', 'peer', 'a.bin'), digest)
            compile(call.call_args.args[-1], 'query', 'exec')
            with patch.object(tl, 'run_pb_exec', return_value=(b'PB_DROP missing\n', b'')):
                self.assertIsNone(tl.received_hash('pb', 'peer', 'a.bin'))


if __name__ == '__main__':
    unittest.main()
