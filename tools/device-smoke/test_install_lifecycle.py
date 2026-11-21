import unittest

import install_lifecycle


class RecordPollTest(unittest.TestCase):
    def test_keeps_one_entry_per_change(self):
        timeline = []
        install_lifecycle.record_poll(timeline, 0.1, 100, 'not-polled')
        install_lifecycle.record_poll(timeline, 0.3, 100, 'not-polled')
        install_lifecycle.record_poll(timeline, 5.2, 200, 'unhealthy')
        install_lifecycle.record_poll(timeline, 5.4, 200, 'unhealthy')
        install_lifecycle.record_poll(timeline, 7.9, 200, 'healthy')
        self.assertEqual(timeline, [
            {'seconds': 0.1, 'pid': 100, 'doctor': 'not-polled'},
            {'seconds': 5.2, 'pid': 200, 'doctor': 'unhealthy'},
            {'seconds': 7.9, 'pid': 200, 'doctor': 'healthy'},
        ])

    def test_never_restored_stays_visible(self):
        timeline = []
        for step in range(3):
            install_lifecycle.record_poll(timeline, step * 0.2, 100, 'not-polled')
        self.assertEqual(timeline, [{'seconds': 0.0, 'pid': 100, 'doctor': 'not-polled'}])


if __name__ == '__main__':
    unittest.main()
