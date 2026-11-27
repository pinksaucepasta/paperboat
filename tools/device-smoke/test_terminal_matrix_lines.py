import os
import shlex
import tempfile
import time
import unittest
from pathlib import Path

import terminal_matrix as matrix
import terminal_matrix_conpty as conpty


class Lines(unittest.TestCase):
    def numbers(self, data, prefix=b'DR'):
        return [int(n) for n in matrix.lines(data, prefix)]

    def test_the_first_line_behind_the_hosts_osc_3008_start_is_found(self):
        # captured on a Windows client (N12, Linux test machine): the start marker ends with ESC \\ and no newline, the end marker follows the last line
        data = b'prompt$ \r\n\x1b]3008;start=1;type=command;cwd=/home/vantage-b\x1b\\' + b''.join(b'DR%d\r\n' % i for i in range(1, 61)) + \
            b'\x1b]3008;end=1;exit=success\x1b\\\x1b]3008;start=2;type=prompt\x1b\\'
        self.assertEqual(list(range(1, 61)), self.numbers(data))

    def test_conpty_repaint_sequences_and_bare_carriage_returns_are_not_text(self):
        data = b'\x1b[?25l\x1b[HDR1\r\n\x1b[2KDR2\r\n\rDR3\r\n\x1b7DR4\r\n\x1b[24;1HDR5\r\n\x1b[15~DR6\r\n'
        self.assertEqual([1, 2, 3, 4, 5, 6], self.numbers(data))

    def test_a_lost_or_doubled_line_is_still_seen(self):
        self.assertEqual([1, 3], self.numbers(b'DR1\r\nDR3\r\n'))
        self.assertEqual([1, 1, 2], self.numbers(b'DR1\r\nDR1\r\nDR2\r\n'))

    def test_text_glued_to_a_line_is_not_a_line(self):
        # no substring search: 'junkDR1' must not count as line 1 (it would hide a line that really went missing)
        self.assertEqual([2], self.numbers(b'junkDR1\r\nDR2\r\n'))
        self.assertEqual([], self.numbers(b'DR1\rDR2\r\n'))  # two lines run together by a lone CR are one non-line, not a pass



class WindowsClientCtrlC(unittest.TestCase):
    def test_events_ignore_everything_but_json_objects(self):
        text = 'noise\n{"flood": 2}\n[1]\n{"sent": 10.5}\n{bad\n{"quiet": 0.2}\n'
        self.assertEqual(matrix.device_events(text), {'flood': 2, 'sent': 10.5, 'quiet': 0.2})

    def test_rows_pass_when_yes_ends_under_five_seconds_and_the_shell_answers(self):
        events = {'flood': 2, 'sent': 100.0, 'quiet': 1.2, 'answered': True, 'gone': 102.4}
        rows = matrix.device_rows(events, 1)
        self.assertEqual([ok for _, ok, _ in rows], [True, True, True, True])
        self.assertIn('2.4 s', rows[1][2])

    def test_rows_fail_when_yes_ends_late_or_never_and_keep_the_same_names(self):
        events = {'flood': 2, 'sent': 100.0, 'quiet': 1.0, 'answered': False}
        late, never = matrix.device_rows(dict(events, gone=106.0), 1), matrix.device_rows(dict(events, gone=None), 1)
        self.assertEqual([ok for _, ok, _ in late], [True, False, True, False])
        self.assertFalse(never[1][1])
        self.assertEqual([n for n, _, _ in late], [n for n, _, _ in never])

    def test_a_driver_error_fails_every_row_with_its_reason(self):
        rows = matrix.device_rows({'error': 'no shell answer in 60 s'}, 0)
        self.assertEqual(len(rows), 4)
        self.assertTrue(all(not ok and 'no shell answer' in detail for _, ok, detail in rows))

    def test_a_flood_that_never_started_fails_the_flood_row(self):
        self.assertFalse(matrix.device_rows({'flood': 1}, 1)[0][1])

    def test_a_yes_that_ended_before_ctrl_c_fails(self):
        rows = matrix.device_rows({'flood': 2, 'sent': 100.0, 'quiet': 1.0, 'answered': True, 'gone': 97.0}, 1)
        self.assertFalse(rows[1][1])
        self.assertIn('before Ctrl-C', rows[1][2])


@unittest.skipIf(os.name == 'nt', 'runs the target-side shell script with a fake pgrep')
class TargetYesWatcher(unittest.TestCase):
    def test_it_arms_on_the_flood_and_reports_the_end_only_when_yes_ends(self):
        with tempfile.TemporaryDirectory() as work:
            count = Path(work) / 'count'

            def set_count(text):  # replaced in one step: a `cat` must never see a half-written file
                new = Path(work) / 'count.new'
                new.write_text(text)
                os.replace(new, count)

            set_count('1')  # one `yes` that was there before the flood
            fake = Path(work) / 'pgrep'
            fake.write_text('#!/bin/sh\ncat %s\n' % shlex.quote(str(count)))
            fake.chmod(0o755)
            path = work + os.pathsep + os.environ['PATH']
            watcher = conpty.YesWatcher(['env', 'PATH=' + path, 'sh', '-c', conpty.watch_script(1)])
            try:
                self.assertIsNone(watcher._line(0.5))  # no new yes yet: neither armed nor an end time
                set_count('2')
                self.assertEqual(watcher.armed(10), 2)
                self.assertIsNone(watcher._line(0.5))  # still running
                before = time.time()
                set_count('1')
                gone = watcher.gone(10)
                self.assertIsNotNone(gone)
                self.assertGreaterEqual(gone, before)
            finally:
                watcher.close()
            set_count('2')
            watcher = conpty.YesWatcher(['env', 'PATH=' + path, 'sh', '-c', conpty.watch_script(1)])
            try:
                self.assertEqual(watcher.armed(10), 2)
                set_count('')  # a count that failed is not an ended `yes`
                self.assertIsNone(watcher.gone(10))
            finally:
                watcher.close()


if __name__ == '__main__':
    unittest.main()
