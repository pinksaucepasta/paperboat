import time
import unittest

import path_display as pd


class Window(unittest.TestCase):
    def test_states_allowed_while_the_relay_is_forced(self):
        for sample in ({'path': 'relay'}, {'path': 'wss'}, {'path': 'none'}, {'error': 'ValueError'},
                       {'path': 'mixed', 'consumers': ['relay', 'wss']}):
            self.assertFalse(pd.wrong_path(sample), sample)

    def test_wrong_states(self):
        for sample in ({'path': 'direct'}, {'path': 'mixed', 'consumers': ['direct', 'relay']},
                       {'path': 'mixed', 'consumers': []}, {'path': None}, {'path': 'peer_relay'}, {}):
            self.assertTrue(pd.wrong_path(sample), sample)

    def test_one_wrong_sample_fails_the_window_even_when_the_relay_shows_later(self):
        samples = [{'t': 0.0, 'path': 'direct'}, {'t': 1.0, 'path': 'none'}, {'t': 2.0, 'path': 'relay'}]
        failures = pd.window_failures(samples)
        self.assertEqual(len(failures), 1)
        self.assertIn('1 of 3 samples while the relay was forced (wrong: direct; window ended on: relay;', failures[0])
        self.assertEqual(pd.window_failures(samples[1:]), [])

    def test_a_window_nobody_could_read_fails(self):
        self.assertTrue(pd.window_failures([{'t': 0.0, 'error': 'ValueError'}]))
        self.assertTrue(pd.window_failures([]))

    def test_the_sampler_records_every_answer_with_its_time(self):
        answers = iter([{'selected_path': 'direct'}, ValueError(),
                        {'selected_path': 'mixed', 'transport_consumers': [{'path': 'relay'}]}])
        last = {'selected_path': 'relay', 'relay_region': 'eu'}

        def read():
            answer = next(answers, last)
            if isinstance(answer, Exception):
                raise answer
            return answer

        sampler = pd.Sampler(read, pause=0.01)
        deadline = time.monotonic() + 10
        while len(sampler.samples) < 5 and time.monotonic() < deadline:
            time.sleep(0.01)
        sampler.stop()
        count = len(sampler.samples)
        self.assertEqual([s.get('path') for s in sampler.samples[:4]], ['direct', None, 'mixed', 'relay'])
        self.assertEqual(sampler.samples[1]['error'], 'ValueError')
        self.assertEqual(sampler.samples[2]['consumers'], ['relay'])
        self.assertTrue(all('t' in s and 'at' in s and 'answered' in s for s in sampler.samples))
        time.sleep(0.05)
        self.assertEqual(len(sampler.samples), count)  # nothing after stop: the relay is no longer forced


if __name__ == '__main__':
    unittest.main()
