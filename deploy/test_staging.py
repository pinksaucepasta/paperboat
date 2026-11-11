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


class BuildVersionTest(unittest.TestCase):
    def test_build_version_matches_the_dockerfile_pattern(self):
        import re
        calls = []
        original = (staging.subprocess.run, staging.ssh, staging.source_archive)
        staging.source_archive = lambda commit: ('abcdef0', 'abcdef0123456789abcdef0123456789abcdef01', b'')
        staging.subprocess.run = lambda *a, **k: type('Done', (), {'returncode': 0, 'stderr': b''})()
        staging.ssh = lambda host, script, **k: calls.append(script)
        try:
            staging.build('HEAD')
        finally:
            staging.subprocess.run, staging.ssh, staging.source_archive = original
        version = re.search(r'PAPERBOAT_VERSION=(\S+)', calls[0]).group(1)
        self.assertRegex(version, r'^[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.(0|[1-9][0-9]*)$')  # server/deploy/Dockerfile's check


if __name__ == '__main__':
    unittest.main()
