import contextlib
import io
import json
from pathlib import Path
import unittest

import account_rows as ar
import preflight_fixtures
import recipe_checks as rc
import recipe_devices as rd
from test_recipe_checks import Fake, check_of

HERE = Path(__file__).resolve().parent
ADD = {'schema_version': '1.0', 'ok': True, 'data': {
    'linux_macos_command': "curl -fsSL https://get.example.test/install.sh | PB_TOKEN='tok-linux-0123456789' sh",
    'windows_command': "$env:PB_TOKEN='tok-linux-0123456789'; irm https://get.example.test/install.ps1 | iex"}}


class Installers(unittest.TestCase):
    def test_the_two_printed_commands_give_the_installer_urls_and_the_token_is_redacted(self):
        self.addCleanup(rd.SECRETS.discard, 'tok-linux-0123456789')
        creator = Fake({('machine', 'add'): rd.Result(0, json.dumps(ADD))})
        self.assertEqual(('https://get.example.test/install.sh', 'https://get.example.test/install.ps1'), ar.installers(creator))
        self.assertNotIn('tok-linux', rd.redact('x tok-linux-0123456789'))

    def test_a_changed_command_is_never_run(self):
        self.addCleanup(rd.SECRETS.discard, 'tok-linux-0123456789')
        bad = dict(ADD, data=dict(ADD['data'], linux_macos_command="curl -fsSL https://x.test | PB_TOKEN='a' sh; rm -rf /"))
        with self.assertRaisesRegex(AssertionError, 'two install commands'):
            ar.installers(Fake({('machine', 'add'): rd.Result(0, json.dumps(bad))}))


class Machines(unittest.TestCase):
    def test_a_machine_that_has_paperboat_is_refused_untouched(self):
        check = check_of(Fake(), 'pair-token-refusals:linux')
        target = Fake(present=['/home/u/.local/bin/pb'])
        original, rc.target_device = rc.target_device, lambda *_: ('m', target)
        self.addCleanup(setattr, rc, 'target_device', original)
        with contextlib.redirect_stderr(io.StringIO()):
            ar.check_token_refusals(check, 'linux')
        self.assertIn('clean-machine-and-tokens', check.steps.result()[1])
        self.assertEqual([], target.calls)

    def test_a_refused_enrollment_token_travels_in_PB_TOKEN(self):
        self.assertNotIn('refuse_install(target', (HERE / 'account_rows.py').read_text())

    def test_only_the_web_servers_it_started_are_stopped(self):
        stopped = []
        device = Fake()
        device.sh = lambda script, **_: stopped.append(script) or rd.Result(0, '')
        web = ar.WebServer(device)
        web.pids = [41, 42]
        web.stop()
        web.stop()
        self.assertEqual(['sudo -n kill 42 2>/dev/null; true', 'sudo -n kill 41 2>/dev/null; true'], stopped)


class Registration(unittest.TestCase):
    smoke = (HERE / 'smoke.sh').read_text()

    def test_every_check_is_a_smoke_row_with_a_fixture_preflight(self):
        listed = ' '.join(line.split('=', 1)[1].strip('"') for line in self.smoke.splitlines()
                          if line.startswith(('ACCOUNT_CHECKS=', 'ACCOUNT_MACHINE_CHECKS=')))
        self.assertEqual(sorted(ar.CHECKS), sorted(listed.split()))
        self.assertIn('\naccount_rows_run\n', self.smoke)
        self.assertIn('harness_checks_run account_rows.py "$ACCOUNT_CHECKS" "$ACCOUNT_MACHINE_CHECKS"', self.smoke)
        for check in ar.CHECKS:
            self.assertIn('fixture_host', preflight_fixtures.NEEDS[check])

    def test_the_real_machine_rows_need_the_clean_machine_setting(self):
        for line in self.smoke.splitlines():
            if line.startswith('ACCOUNT_CHECKS='):
                self.assertTrue(all(i.endswith(':container') for i in line.split('=', 1)[1].strip('"').split()), line)


if __name__ == '__main__':
    unittest.main()
