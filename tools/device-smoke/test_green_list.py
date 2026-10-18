import json
from pathlib import Path
import tempfile
import unittest

import green_list


class GreenListTests(unittest.TestCase):
    def test_only_checks_confined_to_the_target_with_a_runner_are_kept(self):
        self.assertTrue(green_list.involves_only('exec-main:hp-hp', 'hp'))
        self.assertTrue(green_list.involves_only('preview:hp', 'hp'))
        self.assertFalse(green_list.involves_only('exec-main:hp-apple', 'hp'))
        self.assertFalse(green_list.involves_only('preview-browser', 'hp'))
        self.assertTrue(green_list.has_runner('preview-manage:hp-hp', 'test_application_pairs preview-manage x'))
        self.assertFalse(green_list.has_runner('preview-browser:hp-background', 'skipped preview-manage'))

    def test_a_check_that_ever_passed_stays_when_a_later_run_fails_it(self):
        with tempfile.TemporaryDirectory() as directory:
            results = Path(directory)
            for name, status in (('1', 'PASS'), ('2', 'FAIL')):
                (results / (name + '.json')).write_text(json.dumps({'results': [
                    {'id': 'doctor:hp', 'status': 'PASS'}, {'id': 'exec:hp', 'status': status}]}))
            found = green_list.ever_passed(results, 'skipped exec ||', 'hp')
        self.assertEqual(found, {'exec:hp'})


if __name__ == '__main__':
    unittest.main()
