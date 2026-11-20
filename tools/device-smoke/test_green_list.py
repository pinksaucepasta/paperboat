import json
from pathlib import Path
import tempfile
import unittest
import os
from unittest.mock import patch

import green_list


class GreenListTests(unittest.TestCase):
    def setUp(self):
        settings = patch.dict(os.environ, PB_DEV_TARGETS_FILE=str(Path(__file__).with_name('targets.example.json')))
        settings.start()
        self.addCleanup(settings.stop)

    def test_only_checks_confined_to_the_target_with_a_runner_are_kept(self):
        self.assertTrue(green_list.involves_only('exec-main:linux-linux', 'linux'))
        self.assertTrue(green_list.involves_only('preview:linux', 'linux'))
        self.assertFalse(green_list.involves_only('exec-main:linux-macos', 'linux'))
        self.assertFalse(green_list.involves_only('preview-browser', 'linux'))
        self.assertTrue(green_list.has_runner('preview-manage:linux-linux', 'test_application_pairs preview-manage x'))
        self.assertFalse(green_list.has_runner('preview-browser:linux-background', 'skipped preview-manage'))

    def test_a_check_that_ever_passed_stays_when_a_later_run_fails_it(self):
        with tempfile.TemporaryDirectory() as directory:
            results = Path(directory)
            for name, status in (('1', 'PASS'), ('2', 'FAIL')):
                (results / (name + '.json')).write_text(json.dumps({'results': [
                    {'id': 'doctor:linux', 'status': 'PASS'}, {'id': 'exec:linux', 'status': status}]}))
            found = green_list.ever_passed(results, 'skipped exec ||', 'linux')
        self.assertEqual(found, {'exec:linux'})


if __name__ == '__main__':
    unittest.main()
