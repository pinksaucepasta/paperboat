import contextlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest import mock

import console_rows as rows
import fixture_checks
import preflight_fixtures

HERE = Path(__file__).resolve().parent


class Verdict(unittest.TestCase):
    def setUp(self):
        self.kept = []
        patcher = mock.patch.object(fixture_checks, 'keep', lambda check, text: self.kept.append(text) or '/log')
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_every_script_passing_is_a_pass_with_counts(self):
        ok, detail = rows.verdict('x', [('a', 0, 'PASS  one\nPASS  two  (d)\n2/2 passed'), ('b', 0, 'PASS  three\n')])
        self.assertTrue(ok)
        self.assertEqual('a 2/2, b 1/1', detail)

    def test_failures_of_every_script_are_collected(self):
        ok, detail = rows.verdict('x', [('a', 1, 'PASS  one\nFAIL  two\n'), ('b', 1, 'FAIL  three\n')])
        self.assertFalse(ok)
        self.assertIn('a: two', detail)
        self.assertIn('b: three', detail)

    def test_silence_and_a_nonzero_exit_never_pass(self):
        for code, output in ((0, ''), (0, 'SKIP  replay rows  (not set)'), (1, 'PASS  one\n'), (1, 'Error: boom')):
            ok, _ = rows.verdict('x', [('a', code, output)])
            self.assertFalse(ok, (code, output))
        self.assertFalse(rows.verdict('x', [])[0])

    def test_addresses_never_reach_the_receipt_or_the_log(self):
        ok, detail = rows.verdict('x', [('a', 1, 'FAIL  row for someone@example.test missing\nPASS  b@c.example.org\n')])
        self.assertFalse(ok)
        for text in [detail] + self.kept:
            self.assertNotIn('@example', text)
            self.assertIn('<email>', text)


class Skips(unittest.TestCase):
    def test_a_skipped_step_fails_the_row_unless_allowed(self):
        passed, failed = rows.steps('r', 0, 'PASS  one\nSKIP  permission removal closes the replay  (too short)\n')
        self.assertEqual((1, 1), (passed, len(failed)))
        self.assertEqual((1, []), rows.steps('r', 0, 'PASS  one\nSKIP  offline machine state  (OFFLINE_RECORDING not set)\n'))


class Cleanup(unittest.TestCase):
    def machines(self):
        m = fixture_checks.Machines.__new__(fixture_checks.Machines)
        contextlib.ExitStack.__init__(m)
        m.leftovers, m.env = [], {}
        return m

    def test_invitee_is_removed_while_its_directory_still_exists(self):
        seen = []

        def script(m, label, name, *args, state, env=None, timeout=0):
            seen.append((args[0], Path(args[2] if name == 'console_invite_mail.mjs' else args[1]).is_dir(), (env or {}).get('ORIGIN')))
            if args[0] == 'run':
                Path(args[2], 'state.json').write_text('{}')  # the sign-up got that far: an account to delete
            return label, 0, 'PASS  ok\n'

        with mock.patch.object(rows, 'script', script), mock.patch.object(fixture_checks, 'keep', lambda *a: '/log'), \
                mock.patch.object(fixture_checks, 'ENV', {'SMOKE_OPERATOR_STATE': '/s', 'SMOKE_CONSOLE_ORIGIN': 'https://console.test',
                                                          'SMOKE_MAIL_CONSOLE_ORIGIN': 'https://app.test'}):
            with self.machines() as m:
                self.assertTrue(rows.console_invite_mail(m)[0])
        self.assertEqual([('run', True, None), ('cleanup', True, None), ('delete', True, 'https://app.test')], seen)
        self.assertEqual([], m.leftovers)

    def test_a_failed_cancel_is_reported_as_left_behind(self):
        with mock.patch.object(rows, 'script', lambda *a, **k: ('c', 1, 'invitation inv_1 not cancelled')), tempfile.TemporaryDirectory() as directory:
            m = self.machines()
            rows.remove_invitee(m, directory, '/s', 'team')
        self.assertIn('NOT cancelled', m.leftovers[0])


@unittest.skipUnless(shutil.which('node'), 'node is not installed')
class SharedSessionGuard(unittest.TestCase):
    """console_signout_switch.mjs refuses before it loads a browser: the shared operator session is never signed out."""

    def attempt(self, state, shared):
        env = dict(os.environ, STATE=str(state), OPERATOR_STATE=str(shared), ORIGIN='https://console.test', PW_MODULE='/nonexistent')
        return subprocess.run(['node', str(HERE / 'console_signout_switch.mjs'), 'end'], env=env, capture_output=True, text=True, timeout=60)

    def test_the_shared_file_a_link_to_it_and_a_copy_of_it_are_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            shared, copy, link = (Path(directory, n) for n in ('shared.json', 'copy.json', 'link.json'))
            shared.write_text(json.dumps({'cookies': [{'name': '__Host-pb-session', 'value': 'abc'}]}))
            shutil.copyfile(shared, copy)
            link.symlink_to(shared)
            for state in (shared, copy, link):
                done = self.attempt(state, shared)
                self.assertEqual(2, done.returncode, state.name)
                self.assertIn('refused', done.stderr)

    def test_a_session_of_its_own_passes_the_guard(self):
        with tempfile.TemporaryDirectory() as directory:
            shared, own = Path(directory, 'shared.json'), Path(directory, 'own.json')
            shared.write_text(json.dumps({'cookies': [{'name': '__Host-pb-session', 'value': 'abc'}]}))
            own.write_text(json.dumps({'cookies': [{'name': '__Host-pb-session', 'value': 'xyz'}]}))
            self.assertNotIn('refused', self.attempt(own, shared).stderr)  # it fails later, loading the absent browser module


class Registration(unittest.TestCase):
    def test_every_row_runs_in_smoke_and_is_preflighted(self):
        smoke = (HERE / 'smoke.sh').read_text()
        for name in rows.CHECKS:
            self.assertIs(rows.CHECKS[name], fixture_checks.CHECKS[name])
            self.assertIn('\nfixture_check %s\n' % name, smoke)
            self.assertIn(name, preflight_fixtures.NEEDS)
            self.assertIn('console_tools', preflight_fixtures.NEEDS[name])

    def test_the_paste_in_checks_exist(self):
        for name, _ in rows.ADMIN_CHECKS + rows.PAGING_CHECKS:
            self.assertTrue((rows.SCRIPTS / name).is_file(), name)


if __name__ == '__main__':
    unittest.main()
