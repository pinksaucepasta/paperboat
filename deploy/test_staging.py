"""staging.py: the build's source archive is the repository root of the commit (it was the deploy directory: no server/ to build)."""
import importlib.util
import io
import subprocess
import tarfile
import unittest
from pathlib import Path

spec = importlib.util.spec_from_file_location('staging', Path(__file__).with_name('staging.py'))
staging = importlib.util.module_from_spec(spec)
spec.loader.exec_module(staging)


class SourceArchiveTest(unittest.TestCase):
    def test_archive_holds_every_module_the_image_builds_from_and_not_the_excluded_trees(self):
        head = subprocess.run(['git', 'rev-parse', 'HEAD'], capture_output=True, text=True, check=True, cwd=staging.REPO).stdout.strip()
        short, full, tar = staging.source_archive('HEAD')
        self.assertEqual(full, head)
        self.assertTrue(full.startswith(short))
        names = tarfile.open(fileobj=io.BytesIO(tar)).getnames()
        for required in ('server/deploy/Dockerfile', 'go.work', 'shared', 'cli', 'deploy/staging.py'):
            self.assertIn(required, names)
        self.assertFalse([n for n in names if n.split('/')[0] in ('console', 'marketing', 'tools')])


if __name__ == '__main__':
    unittest.main()
