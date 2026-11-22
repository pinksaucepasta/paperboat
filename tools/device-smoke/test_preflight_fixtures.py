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


class TestFixturesEndpoint(unittest.TestCase):
    """The recording-key-rotation fixture: the server's test-fixtures endpoint answers to our token."""

    def verify(self, reply, mode=0o600, token='secret-token'):
        import tempfile
        import urllib.error
        with tempfile.TemporaryDirectory() as directory:
            file = f'{directory}/token'
            with open(file, 'w') as out:
                out.write(token)
            import os
            os.chmod(file, mode)
            settings = {'SMOKE_FIXTURES_URL': 'https://api.example.test/v1/test-fixtures', 'SMOKE_FIXTURES_TOKEN_FILE': file}
            seen = []

            def urlopen(request, timeout):
                seen.append((request.full_url, request.get_header('Authorization')))
                if isinstance(reply, int):
                    raise urllib.error.HTTPError(request.full_url, reply, 'x', {}, None)
                return mock.Mock()
            with mock.patch.dict(pf.ENV, settings), mock.patch('urllib.request.urlopen', urlopen):
                return pf.test_fixtures(), seen

    def test_accepted_token_is_ok_and_sent_only_as_a_bearer(self):
        problem, seen = self.verify(None)
        self.assertIsNone(problem)
        self.assertEqual([('https://api.example.test/v1/test-fixtures/zone', 'Bearer secret-token')], seen)

    def test_dns_not_configured_and_rate_limit_still_mean_the_token_works(self):
        self.assertIsNone(self.verify(503)[0])
        self.assertIsNone(self.verify(429)[0])

    def test_each_refusal_names_its_fix_and_never_the_token(self):
        self.assertIn('rejects the token', self.verify(401)[0])
        self.assertIn('PAPERBOAT_TEST_FIXTURES_TOKEN is not set', self.verify(404)[0])
        self.assertIn('mode 0600', self.verify(None, mode=0o644)[0])
        self.assertIn('empty', self.verify(None, token='\n')[0])
        for problem in (self.verify(401)[0], self.verify(404)[0]):
            self.assertNotIn('secret-token', problem)

    def test_unset_settings_are_reported(self):
        with mock.patch.dict(pf.ENV, {}, clear=True):
            self.assertIn('SMOKE_FIXTURES_URL', pf.test_fixtures())

    def test_the_check_needs_no_permanent_accounts(self):
        self.assertEqual(['test_fixtures', 'console_tools', 'fixture_host'], pf.NEEDS['recording-key-rotation'])


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
