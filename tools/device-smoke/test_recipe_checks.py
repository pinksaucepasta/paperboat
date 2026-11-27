import contextlib
import io
import json
from pathlib import Path
import shlex
import unittest
from unittest.mock import patch

import preflight_fixtures
import recipe_checks as rc
import recipe_devices as rd

HERE = Path(__file__).resolve().parent
TOKEN = 'pbr_' + 'abcde23456' + '_' + 'k7' * 26


class Fake:
    """A device whose pb answers from a script: {first words: Result or a list of Results used in turn}."""

    os, kind = 'linux', 'container'

    def __init__(self, answers=None, present=()):
        self.answers, self.found, self.calls = answers or {}, list(present), []

    def pb(self, *args, stdin=None, timeout=180):
        self.calls.append(args)
        for size in range(len(args), 0, -1):
            if args[:size] in self.answers:
                answer = self.answers[args[:size]]
                return answer.pop(0) if isinstance(answer, list) else answer
        return rd.Result(1, '', 'unexpected ' + ' '.join(args))

    def present(self):
        return self.found

    def hooks(self):
        return []

    def write(self, filename, text):
        self.calls.append(('write', filename, text))
        return '/w/' + filename

    def one_liner(self, *args, **kwargs):
        raise AssertionError('the installer must not run here')

    def uninstall(self):
        raise AssertionError('uninstall must not run here')


class FakeSession:
    run_id, vault, marker, creator_name, password = 'ab12', 'rcp-vault', 'rcpvalueX', 'test-creator', 'pw'

    def __init__(self, creator):
        self.creator = creator

    def name(self, role):
        return 'test-1127-rcp-%s-%s' % (role, self.run_id)

    def vault_ready(self):
        pass


def check_of(creator, check_id='recipe-lifecycle'):
    steps = rc.Steps(check_id)
    return rc.Check(FakeSession(creator), steps, contextlib.ExitStack())


class Secrets(unittest.TestCase):
    def test_redaction_covers_tokens_passwords_and_sign_in_codes(self):
        rd.SECRETS.add('enrol-me-0123456789')
        try:
            said = rd.redact('t %s e enrol-me-0123456789 pbat_%s https://x/cli/authorize?code=L5ZW-5ZZB' % (TOKEN, 'a' * 40))
        finally:
            rd.SECRETS.discard('enrol-me-0123456789')
        for secret in (TOKEN.split('_')[2], 'enrol-me', 'a' * 40, 'L5ZW-5ZZB'):
            self.assertNotIn(secret, said)

    def test_a_failed_step_never_records_the_token(self):
        steps = rc.Steps('x')
        with contextlib.redirect_stderr(io.StringIO()), steps.step('leaks'):
            raise RuntimeError('create said ' + TOKEN)
        self.assertNotIn(TOKEN.split('_')[2], steps.result()[1] + ''.join(steps.log))

    def test_shown_once_wants_exactly_one_token(self):
        self.assertEqual(TOKEN, rc.shown_once('Token (shown once):\n  %s\n' % TOKEN))
        for output in ('', TOKEN + '\n' + TOKEN):
            with self.assertRaises(AssertionError):
                rc.shown_once(output)

    def test_the_secret_goes_on_stdin_never_in_the_command(self):
        device = rd.Posix('container', 'host', 'box')
        with patch.object(rd, 'spawn', return_value=rd.Result(0, '')) as spawn:
            device.one_liner('https://get.example.test', 'PB_RECIPE', TOKEN, 'm1', '2026.01.02.3')
        argv, stdin = spawn.call_args[0][0], spawn.call_args[0][1]
        self.assertNotIn(TOKEN, ' '.join(argv))
        self.assertEqual(TOKEN + '\n', stdin)
        self.assertIn('PB_VERSION=2026.01.02.3', argv[-1])


class InstallLines(unittest.TestCase):
    def test_the_printed_lines_are_accepted(self):
        self.assertEqual('https://get.example.test', rd.installer_url('curl -fsSL https://get.example.test | sh'))
        self.assertEqual('https://get.example.test', rd.installer_url('irm https://get.example.test | iex', windows=True))

    def test_anything_else_from_the_server_is_not_run(self):
        for line in ('curl -fsSL http://get.example.test | sh', 'curl -fsSL https://get.example.test | sh; rm -rf /',
                     'curl -fsSL https://get.example.test$(id) | sh', 'sh -c true', ''):
            with self.assertRaises(AssertionError, msg=line):
                rd.installer_url(line)

    def test_one_liner_script_is_one_quoted_shell_script(self):
        script = rd.Posix('linux', 'h').one_liner_script('https://get.example.test', 'PB_RECIPE', "a'b")
        self.assertIn('IFS= read -r v', script)
        self.assertIn(shlex.quote("a'b"), script)
        with self.assertRaises(ValueError):
            rd.Posix('linux', 'h').one_liner_script('https://x', 'PATH', 'n')


class Collect(unittest.TestCase):
    def test_every_step_runs_after_a_failure(self):
        steps, ran = rc.Steps('x'), []
        with contextlib.redirect_stderr(io.StringIO()):
            for name in ('one', 'two', 'three'):
                with steps.step(name):
                    ran.append(name)
                    rc.need(name == 'two', name + ' broke')
        ok, detail = steps.result()
        self.assertEqual(['one', 'two', 'three'], ran)
        self.assertFalse(ok)
        self.assertIn('one, three', detail)

    def test_no_step_is_not_a_pass(self):
        self.assertFalse(rc.Steps('x').result()[0])

    def test_refused_needs_a_failure_with_the_reason(self):
        rc.refused(rd.Result(1, '', 'pb: This recipe has\n reached its max_machines.'), 'x', rc.FULL)
        for result in (rd.Result(0, rc.FULL), rd.Result(124, rc.FULL), rd.Result(1, 'sudo: a password is required')):
            with self.assertRaises(AssertionError):
                rc.refused(result, 'x', rc.FULL)

    def test_wait_for_reports_the_last_reason(self):
        with patch.object(rc.time, 'sleep'), patch.object(rc.time, 'monotonic', side_effect=[0, 1, 100]):
            with self.assertRaisesRegex(AssertionError, 'within 10 s: still listed'):
                rc.wait_for('gone', lambda: rc.need(False, 'still listed'), 10)


class NeverTouchesAMachineWithPaperboat(unittest.TestCase):
    def test_a_machine_that_has_paperboat_is_left_alone(self):
        creator = Fake()
        check = check_of(creator, 'recipe-install:linux')
        target = Fake(present=['/home/u/.local/bin/pb'])
        with patch.object(rc, 'target_device', return_value=('m', target)), contextlib.redirect_stderr(io.StringIO()):
            rc.check_install(check, 'linux')
        ok, detail = check.steps.result()
        self.assertFalse(ok)
        self.assertIn('target-has-no-paperboat', detail)
        self.assertEqual([], target.calls)
        self.assertEqual([], creator.calls)

    def test_a_paired_smoke_machine_or_a_personal_pc_is_refused(self):
        check = check_of(Fake())
        for env, text in (({'SMOKE_RECIPE_WINDOWS_SSH': 'u@win', 'SMOKE_WINDOWS_SSH': 'u@win'}, 'paired smoke machine'),
                          ({'SMOKE_RECIPE_WINDOWS_SSH': 'someone@victus'}, 'personal computer'), ({}, 'is not set')):
            with patch.dict(rc.f.ENV, env, clear=True), self.assertRaisesRegex(AssertionError, text):
                rc.target_device(check, 'windows')

    def test_a_leftover_shell_hook_is_not_a_clean_machine(self):
        check = check_of(Fake(), 'recipe-install:linux')
        target = Fake()
        target.hooks = lambda: ['/home/u/.bashrc']
        with patch.object(rc, 'target_device', return_value=('m', target)), contextlib.redirect_stderr(io.StringIO()):
            rc.check_install(check, 'linux')
        self.assertIn('target-has-no-paperboat', check.steps.result()[1])
        self.assertEqual([], target.calls)

    def test_present_needs_the_whole_answer(self):
        with patch.object(rd, 'spawn', return_value=rd.Result(255, '', 'ssh: no route')):
            with self.assertRaises(AssertionError):
                rd.Posix('linux', 'h').present()
        with patch.object(rd, 'spawn', return_value=rd.Result(0, 'END\n')):
            self.assertEqual([], rd.Posix('linux', 'h').present())


class Cleanup(unittest.TestCase):
    def test_every_live_recipe_is_revoked_with_its_code(self):
        listed = rd.Result(0, json.dumps({'schema_version': '1.0', 'ok': True, 'data': {'recipes': [{'name': 'r1'}, {'name': 'r2'}]}}))
        creator = Fake({('recipe', 'list', '--json'): listed,
                        ('recipe', 'revoke', 'r1'): [rd.Result(0, 'Revoke with: pb recipe revoke r1 --confirm AB12CD\n')],
                        ('recipe', 'revoke', 'r1', '--confirm', 'AB12CD'): rd.Result(0, 'revoked'),
                        ('recipe', 'revoke', 'r2'): [rd.Result(0, 'Revoke with: pb recipe revoke r2 --confirm EF34GH\n')],
                        ('recipe', 'revoke', 'r2', '--confirm', 'EF34GH'): rd.Result(0, 'revoked')})
        check = check_of(creator)
        check.live = [(creator, 'r1'), (creator, 'r2'), (creator, 'never-created')]
        check.revoke_all()
        self.assertEqual([], check.live)
        self.assertEqual([], check.steps.failed)

    def test_a_failed_revoke_fails_the_check_and_the_others_still_run(self):
        listed = rd.Result(0, json.dumps({'recipes': [{'name': 'r1'}, {'name': 'r2'}]}))
        creator = Fake({('recipe', 'list', '--json'): listed, ('recipe', 'revoke', 'r1'): rd.Result(1, '', 'offline'),
                        ('recipe', 'revoke', 'r2'): rd.Result(0, 'pb recipe revoke r2 --confirm AB12CD'),
                        ('recipe', 'revoke', 'r2', '--confirm', 'AB12CD'): rd.Result(0, 'ok')})
        check = check_of(creator)
        check.live = [(creator, 'r2'), (creator, 'r1')]
        check.revoke_all()
        self.assertEqual(['cleanup:r1'], [name for name, _ in check.steps.failed])
        self.assertIn(('recipe', 'revoke', 'r2', '--confirm', 'AB12CD'), creator.calls)


class Receipts(unittest.TestCase):
    def test_a_failed_setup_fails_every_requested_check_by_name(self):
        with patch.object(rc.f, 'Machines', side_effect=RuntimeError('no mailbox')), patch.object(rc.f, 'keep', return_value='log'), \
                contextlib.redirect_stdout(io.StringIO()) as out:
            code = rc.main(['recipe_checks.py', 'recipe-lifecycle,recipe-vaults'])
        rows = [json.loads(line) for line in out.getvalue().splitlines()]
        self.assertEqual(1, code)
        self.assertEqual(['recipe-lifecycle', 'recipe-vaults'], [r['check'] for r in rows])
        self.assertTrue(all(r['status'] == 'FAIL' and 'no mailbox' in r['detail'] for r in rows))

    def test_unknown_check_is_usage(self):
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(2, rc.main(['recipe_checks.py', 'recipe-nope']))

    def test_json_data_reads_both_shapes(self):
        self.assertEqual({'a': 1}, rd.json_data(rd.Result(0, '{"schema_version":"1.0","ok":true,"data":{"a":1}}')))
        self.assertEqual({'a': 1}, rd.json_data(rd.Result(0, '{\n "a": 1\n}\n')))
        with self.assertRaises(AssertionError):
            rd.json_data(rd.Result(1, '', 'pb: nope'))


class Registration(unittest.TestCase):
    smoke = (HERE / 'smoke.sh').read_text()

    def test_every_check_is_a_smoke_row_with_a_fixture_preflight(self):
        listed = ' '.join(line.split('=', 1)[1].strip('"') for line in self.smoke.splitlines()
                          if line.startswith(('RECIPE_CHECKS=', 'RECIPE_MACHINE_CHECKS=')))
        self.assertEqual(sorted(rc.CHECKS), sorted(listed.split()))
        self.assertIn('\nrecipe_checks_run\n', self.smoke)
        for check in rc.CHECKS:
            self.assertIn('fixture_host', preflight_fixtures.NEEDS[check])

    def test_the_driver_needs_the_clean_machine_setting(self):
        self.assertEqual({'linux', 'macos', 'windows'}, set(rc.SSH_SETTING))
        self.assertIn('setting=SMOKE_RECIPE_${os^^}_SSH', self.smoke)

    def test_the_bare_container_action_exists(self):
        script = (HERE / 'fixture_machine.sh').read_text()
        self.assertIn('  bare) bare ;;', script)


if __name__ == '__main__':
    unittest.main()
