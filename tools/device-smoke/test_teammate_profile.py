import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

from teammate_profile import environment, prepare


class TeammateProfileTest(unittest.TestCase):
    def test_child_profile_keeps_primary_credentials_and_security_policy(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / 'teammate'
            original = dict(os.environ)
            config = prepare(root, {'server_url': 'https://api.example.test',
                                    'auth': {'allow_file_fallback': False}})
            env = environment(root)
            child = subprocess.run([sys.executable, '-c',
                'import json,os; print(json.dumps({k:os.environ[k] for k in '
                '["XDG_CONFIG_HOME","XDG_DATA_HOME","XDG_STATE_HOME","XDG_CACHE_HOME"]}))'],
                env=env, capture_output=True, text=True, check=True)
            self.assertEqual(json.loads(child.stdout), {
                'XDG_CONFIG_HOME': str(root / 'config'),
                'XDG_DATA_HOME': str(root / 'data'),
                'XDG_STATE_HOME': str(root / 'state'),
                'XDG_CACHE_HOME': str(root / 'cache')})
            self.assertEqual(dict(os.environ), original)
            saved = json.loads(config.read_text())
            self.assertFalse(saved['auth']['allow_file_fallback'])
            self.assertEqual(saved['auth']['profile_dir'], str(root / 'credentials'))
            self.assertEqual(config.stat().st_mode & 0o777, 0o600)


if __name__ == '__main__':
    unittest.main()
