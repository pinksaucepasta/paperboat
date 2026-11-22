"""preflight_fixtures.py: which fixtures a run needs, and one report naming every missing one."""
import io
import unittest
from contextlib import redirect_stderr
from unittest import mock

import preflight_fixtures as pf


class Args:
    def __init__(self, checks='', only_ids='', skip='', windows=False):
        self.checks, self.only_ids, self.skip, self.windows = checks, only_ids, skip, windows


class Selection(unittest.TestCase):
    def test_everything_by_default(self):
        self.assertEqual(list(pf.NEEDS), pf.selected(Args()))

    def test_only_list_and_skip_narrow_it(self):
        self.assertEqual(['free-plan'], pf.selected(Args(only_ids='|free-plan|exec|')))
        self.assertEqual(['free-plan'], pf.selected(Args(only_ids='|free-plan:hp|')))  # an id with a device suffix selects its group
        self.assertEqual(['team-business'], pf.selected(Args(checks='free-plan,team-business', skip='free-plan')))

    def test_windows_run_has_no_docker_or_free_fixture(self):
        self.assertEqual({'ssh-rsync', 'config-team-default', 'team-business'}, set(pf.selected(Args(windows=True))))
        self.assertNotIn('fixture_host', sum(pf.NEEDS_WINDOWS.values(), []))

    def test_every_need_has_a_verifier(self):
        for needs in list(pf.NEEDS.values()) + list(pf.NEEDS_WINDOWS.values()):
            for name in needs:
                self.assertIn(name, pf.VERIFIERS)


class Report(unittest.TestCase):
    def run_main(self, **verifiers):
        patched = {name: (lambda value=value: value) for name, value in verifiers.items()}
        stderr = io.StringIO()
        with mock.patch.dict(pf.VERIFIERS, patched), redirect_stderr(stderr):
            code = pf.main(['--checks', 'free-plan,config-team-default'])
        return code, stderr.getvalue()

    def test_all_present_is_silent_success(self):
        code, text = self.run_main(free_session=None, free_plan=None, fixture_host=None, operator_session=None, config_repo=None, team_admin=None)
        self.assertEqual(0, code)
        self.assertNotIn('MISSING', text)

    def test_every_missing_fixture_is_named_once_with_its_fix_and_nothing_starts(self):
        code, text = self.run_main(free_session='Free session expired. Fix: sign in', free_plan=None, fixture_host='no docker host', operator_session=None,
                                   config_repo='repo not connected. Fix: connect it', team_admin=None)
        self.assertEqual(2, code)
        self.assertIn('free-plan', text)
        self.assertIn('Free session expired. Fix: sign in', text)
        self.assertIn('repo not connected. Fix: connect it', text)
        self.assertEqual(2, text.count('no docker host'))  # a shared fixture is reported under each check that needs it
        self.assertIn('nothing was started', text)


if __name__ == '__main__':
    unittest.main()
