"""Exercise the gate's real receipt evaluator against committed source trees."""
import json
from pathlib import Path
import re
import subprocess
import tempfile
import unittest


GATE = Path(__file__).resolve().parents[1] / 'gate.sh'


class GateReceipts(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.git('init', '-q')
        self.git('config', 'user.name', 'Gate test')
        self.git('config', 'user.email', 'gate@example.test')
        (self.root / 'cli').mkdir()
        (self.root / 'cli/source.go').write_text('package example\n')
        self.commit('runtime')
        self.version = 'dev-' + self.git('rev-parse', '--short', 'HEAD').strip()
        self.results = self.root / 'tools/device-smoke/results'
        self.results.mkdir(parents=True)

    def git(self, *args):
        return subprocess.check_output(['git', *args], cwd=self.root, text=True)

    def commit(self, message):
        self.git('add', 'cli')
        if (self.root / 'AGENTS.md').exists():
            self.git('add', 'AGENTS.md')
        self.git('commit', '-qm', message)

    def receipt(self, name, results, version=None):
        (self.results / (name + '.json')).write_text(json.dumps({
            'expect_version': version or self.version,
            'results': [{'id': key, 'status': value} for key, value in results.items()]}))

    def evaluate(self, script=None):
        source = script or GATE.read_text()
        owner = re.search(r"python3 - .*?<<'PY'.*?\n(.*?)\nPY", source, re.S).group(1)
        want = 'dev-' + self.git('rev-parse', '--short', 'HEAD').strip()
        return subprocess.run(['python3', '-', want], input=owner, cwd=self.root,
                              capture_output=True, text=True)

    def test_documentation_and_focused_rerun_preserve_all_checks(self):
        self.receipt('01', {'exec:hp': 'PASS', 'terminal-new:hp': 'PASS'})
        self.receipt('02', {'exec:hp': 'PASS', 'terminal-new:hp': 'FAIL'})
        self.receipt('03', {'terminal-new:hp': 'PASS'})
        (self.root / 'AGENTS.md').write_text('Batch smoke; focused reruns.\n')
        self.commit('documentation')
        self.assertEqual(self.evaluate().returncode, 0)

    def test_focused_failure_cannot_hide_behind_older_pass(self):
        self.receipt('01', {'exec:hp': 'PASS', 'ssh:hp': 'PASS'})
        self.receipt('02', {'exec:hp': 'FAIL'})
        result = self.evaluate()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('exec:hp', result.stdout)

    def test_changed_runtime_cannot_reuse_old_receipt(self):
        self.receipt('01', {'exec:hp': 'PASS'})
        (self.root / 'cli/source.go').write_text('package changed\n')
        self.commit('runtime changed')
        self.assertNotEqual(self.evaluate().returncode, 0)

    def test_partial_candidate_cannot_omit_historical_check(self):
        self.receipt('01', {'exec:hp': 'PASS', 'ssh:hp': 'PASS'}, 'release-old')
        self.receipt('02', {'exec:hp': 'PASS'})
        result = self.evaluate()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('ssh:hp', result.stdout)


if __name__ == '__main__':
    unittest.main()
