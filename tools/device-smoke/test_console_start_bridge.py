import io
import json
import pathlib
import tempfile
import threading
import unittest
from contextlib import redirect_stdout
from unittest import mock

import console_start_bridge as bridge


class Bridge(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        patch = mock.patch.object(bridge, 'DIR', pathlib.Path(self.tmp.name) / 'bridge')
        patch.start()
        self.addCleanup(patch.stop)
        self.addCleanup(self.tmp.cleanup)

    def claimed(self):
        out = io.StringIO()
        with redirect_stdout(out):
            self.assertEqual(0, bridge.main(['claim']))
        return [json.loads(line) for line in out.getvalue().splitlines()]

    def test_a_request_is_claimed_once_and_its_answer_reaches_the_asker(self):
        result = []
        asker = threading.Thread(target=lambda: result.append(bridge.ask('team-1', 'sess.7', seconds=20)))
        asker.start()
        for _ in range(200):
            found = self.claimed()
            if found:
                break
            threading.Event().wait(0.05)
        self.assertEqual([('team-1', 'sess.7')], [(r['team'], r['session']) for r in found])
        self.assertEqual([], self.claimed())  # removed when claimed
        with mock.patch('sys.stdin', io.StringIO('the console refused: recording_unavailable')):
            self.assertEqual(0, bridge.main(['answer', found[0]['id'], '1']))
        asker.join(10)
        self.assertEqual([(1, 'the console refused: recording_unavailable')], result)
        self.assertEqual([], list(bridge.DIR.glob('*.res')))  # the answer is consumed

    def test_unanswered_request_times_out_and_leaves_nothing_to_claim(self):
        self.assertEqual((1, 'the controller did not answer the console start within 1 s'), bridge.ask('t', 's', seconds=1))
        self.assertEqual([], self.claimed())

    def test_names_that_could_reach_a_command_line_are_refused(self):
        for team, session in (('t; rm -rf /', 's'), ('t', '$(x)'), ('t', '')):
            self.assertEqual(2, bridge.ask(team, session)[0])
        self.assertEqual(2, bridge.main(['--team', 't', '--session']))

    def test_answer_needs_a_real_request_id(self):
        with self.assertRaises(ValueError):
            bridge.answer('../../etc/x', 0, '')


if __name__ == '__main__':
    unittest.main()
