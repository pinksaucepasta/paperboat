"""Checks must not install, pair, reset or uninstall pb in the controller's own home."""
import os
import pwd
import tempfile
import unittest
from unittest.mock import patch

import pairing_lifecycle
import winhost

REAL = pwd.getpwuid(os.getuid()).pw_dir


class RealHomeGuard(unittest.TestCase):
    def refused(self, arguments, **env):
        try:
            winhost.refuse_real_home(arguments, env)
        except RuntimeError:
            return True
        return False

    def test_every_setup_command_is_refused_in_the_real_home(self):
        for arguments in (['install', '--json'], ['pair'], ['reset'], ['uninstall'], ['logout'], ['auth', 'login'],
                          ['service', 'uninstall'], ['--workspace', 'personal', 'reset']):
            self.assertTrue(self.refused(arguments, HOME=REAL), arguments)
            self.assertTrue(self.refused(arguments), arguments)
        for arguments in (['machine', 'list'], ['env', 'reset'], ['exec', 'm', '--', 'reset'], ['--version']):
            self.assertFalse(self.refused(arguments, HOME=REAL), arguments)

    def test_own_home_or_explicit_permission_is_accepted(self):
        with tempfile.TemporaryDirectory() as home:
            self.assertFalse(self.refused(['reset'], HOME=home))
            self.assertTrue(self.refused(['reset'], HOME=home, XDG_CONFIG_HOME=REAL + '/.config'))
            link = os.path.join(home, 'link')
            os.symlink(REAL, link)
            self.assertTrue(self.refused(['reset'], HOME=link))
        self.assertFalse(self.refused(['reset'], HOME=REAL, PB_SMOKE_ALLOW_REAL_HOME='1'))

    def test_no_local_path_starts_pb(self):
        environment = {k: v for k, v in os.environ.items() if not k.startswith('XDG_') and k != 'PB_SMOKE_ALLOW_REAL_HOME'}
        environment['HOME'] = REAL
        started = AssertionError('pb was started')
        with patch.dict(os.environ, environment, clear=True), patch('subprocess.run', side_effect=started), \
                patch('subprocess.Popen', side_effect=started), patch('terminal_command.run', side_effect=started):
            for call in (lambda: pairing_lifecycle.command_result('/pb', 'reset'),
                         lambda: pairing_lifecycle.command_result('/pb', 'reset', terminal=True),
                         lambda: pairing_lifecycle.interrupt_pair('/pb', ['pair'], None, 'sigint', None),
                         lambda: winhost.pb_text('/pb', ['uninstall']),
                         lambda: winhost.run_pb('/pb', ['uninstall'], 5)):
                with self.assertRaisesRegex(RuntimeError, 'refusing'):
                    call()


if __name__ == '__main__':
    unittest.main()
