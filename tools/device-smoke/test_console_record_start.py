"""console_record_start.mjs: syntax, --print-plan and argument validation, without a browser."""
import json, shutil, subprocess, unittest
from pathlib import Path

SCRIPT = str(Path(__file__).with_name('console_record_start.mjs'))
NODE = shutil.which('node')


def run(*args):
    return subprocess.run([NODE, SCRIPT, *args], capture_output=True, text=True, timeout=30)


@unittest.skipUnless(NODE, 'node is not installed')
class ConsoleRecordStart(unittest.TestCase):
    def test_syntax(self):
        self.assertEqual(subprocess.run([NODE, '--check', SCRIPT], capture_output=True, text=True).returncode, 0)

    def test_print_plan_names_the_selectors_and_opens_no_browser(self):
        result = run('--team', 't1', '--session', 'abc-1', '--print-plan')
        self.assertEqual(result.returncode, 0, result.stderr)
        plan = json.loads(result.stdout)
        self.assertEqual((plan['team'], plan['session']), ('t1', 'abc-1'))
        self.assertIn('data-terminal-recording', plan['selectors']['active'])
        self.assertEqual(plan['selectors']['start']['name'], 'Start recording')

    def test_missing_or_bad_arguments_exit_2(self):
        for args in ([], ['--team', 't1'], ['--session', 'x'], ['--team', 't1', '--session', '../x']):
            self.assertEqual(run(*args).returncode, 2, args)

    def test_missing_env_fails_with_one_reason_line(self):
        result = subprocess.run([NODE, SCRIPT, '--team', 't1', '--session', 'x'], capture_output=True, text=True, env={'PATH': '/usr/bin:/bin'})
        self.assertEqual(result.returncode, 1)
        self.assertEqual(len(result.stderr.strip().splitlines()), 1)


if __name__ == '__main__':
    unittest.main()
