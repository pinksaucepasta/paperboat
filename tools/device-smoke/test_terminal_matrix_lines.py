import unittest

import terminal_matrix as matrix


class Lines(unittest.TestCase):
    def numbers(self, data, prefix=b'DR'):
        return [int(n) for n in matrix.lines(data, prefix)]

    def test_the_first_line_behind_the_hosts_osc_3008_start_is_found(self):
        # captured on a Windows client (N12, hp): the start marker ends with ESC \\ and no newline, the end marker follows the last line
        data = b'prompt$ \r\n\x1b]3008;start=1;type=command;cwd=/home/anvit\x1b\\' + b''.join(b'DR%d\r\n' % i for i in range(1, 61)) + \
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


if __name__ == '__main__':
    unittest.main()
