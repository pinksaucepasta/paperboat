import json
import re
import subprocess
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
KEY = re.compile(r"""(?:get_setting|configured_path)\(\s*(?:settings,\s*)?['"]([a-z_]+)['"]|settings\.get\(\s*['"]([a-z_]+)['"]|dev_targets\.py"?\s+(?:"?\$\w+"?\s+)?([a-z_]+)(?=[\s)`"])""")


class ExampleTargets(unittest.TestCase):
    def setUp(self):
        self.example = json.loads((HERE / 'targets.example.json').read_text())

    def test_every_setting_the_tools_read_is_in_the_example(self):
        files = subprocess.check_output(['git', 'ls-files', '-co', '--exclude-standard', '*.py', '*.sh'], cwd=ROOT, text=True).split()
        used = set()
        for name in files:
            if name.endswith('test_dev_targets.py') or not (ROOT / name).is_file():
                continue
            for match in KEY.finditer((ROOT / name).read_text(errors='ignore')):
                used.update(key for key in match.groups() if key)
        # 'smoke' and 'ssh' are dev_targets.py subcommands, 'run' and 'if' are shell words, windows_name is derived.
        missing = used - set(self.example) - {'smoke', 'ssh', 'run', 'if', 'windows_name'}
        self.assertEqual(missing, set())

    def test_example_logins_and_urls_use_the_test_domain(self):
        for text in re.findall(r'"([^"]*(?:@|://)[^"]*)"', json.dumps(self.example)):
            self.assertTrue(text.split('@')[-1].removeprefix('https://').endswith('example.test'), text)


if __name__ == '__main__':
    unittest.main()
