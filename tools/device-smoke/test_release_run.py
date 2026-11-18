import hashlib
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import release_run

VERSION = '2026.10.19.17'


class ReleaseRunTest(unittest.TestCase):
    """A throwaway key signs SHA256SUMS; the module under test trusts the key it is given in place of the embedded one."""

    def setUp(self):
        self.root = Path(tempfile.mkdtemp())
        self.addCleanup(lambda: __import__('shutil').rmtree(self.root, True))
        subprocess.run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', str(self.root / 'key')], check=True)
        public = ' '.join((self.root / 'key.pub').read_text().split()[:2])
        installer = self.root / 'install.sh'
        installer.write_text(f"allowed_signers='paperboat-release namespaces=\"paperboat-release\" {public}'\n")
        patch = mock.patch.object(release_run, 'INSTALLER', installer)
        patch.start()
        self.addCleanup(patch.stop)

    def release(self, assets, version=VERSION):
        sums = f'# paperboat-release {version}\n' + ''.join(f'{hashlib.sha256(data).hexdigest()}  pb-{name}\n' for name, data in assets.items())
        (self.root / 'SHA256SUMS').write_text(sums)
        (self.root / 'SHA256SUMS.sig').unlink(missing_ok=True)
        subprocess.run(['ssh-keygen', '-q', '-Y', 'sign', '-f', str(self.root / 'key'), '-n', 'paperboat-release', str(self.root / 'SHA256SUMS')], check=True)

    def serve(self, assets):
        def fetch(url, limit):
            name = url.rsplit('/', 1)[1]
            if name in ('SHA256SUMS', 'SHA256SUMS.sig'):
                return (self.root / name).read_bytes()
            return assets[name[3:]]
        return mock.patch.object(release_run, 'fetch', fetch)

    def test_downloads_exactly_the_signed_release(self):
        assets = {'linux-amd64': b'amd64 bytes', 'linux-arm64': b'arm64 bytes'}
        self.release(assets)
        with self.serve(assets), tempfile.TemporaryDirectory() as d:
            files = release_run.download(VERSION, 'example/paperboat', Path(d))
            self.assertEqual(files['linux-arm64'].read_bytes(), b'arm64 bytes')

    def test_refuses_sums_signed_for_another_release(self):
        assets = {'linux-amd64': b'a', 'linux-arm64': b'b'}
        self.release(assets, version='2026.10.19.18')
        with self.serve(assets), tempfile.TemporaryDirectory() as d:
            with self.assertRaisesRegex(SystemExit, 'not signed for 2026.10.19.17'):
                release_run.download(VERSION, 'example/paperboat', Path(d))

    def test_refuses_edited_sums_and_bytes_that_do_not_match(self):
        assets = {'linux-amd64': b'good', 'linux-arm64': b'good'}
        self.release(assets)
        with self.serve({'linux-amd64': b'evil', 'linux-arm64': b'good'}), tempfile.TemporaryDirectory() as d:
            with self.assertRaisesRegex(SystemExit, 'SHA-256'):
                release_run.download(VERSION, 'example/paperboat', Path(d))
        sums = self.root / 'SHA256SUMS'
        sums.write_text(sums.read_text().replace('0', '1', 1))
        with self.serve(assets), tempfile.TemporaryDirectory() as d:
            with self.assertRaisesRegex(SystemExit, 'signature'):
                release_run.download(VERSION, 'example/paperboat', Path(d))

    def test_refuses_plain_http(self):
        with self.assertRaisesRegex(SystemExit, 'non-https'):
            release_run.fetch('http://example.test/SHA256SUMS', 10)

    def test_the_run_executes_every_check(self):
        for flag in ('--skip', '--only-check', '--only-list'):
            with self.subTest(flag=flag), mock.patch.object(sys, 'argv', ['x', '--version', '1', '--', flag, 'a']):
                with self.assertRaisesRegex(SystemExit, 'every check'):
                    release_run.main()

    def test_summary_counts_results(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / 'r.json'
            path.write_text(json.dumps({'results': [{'id': 'a', 'status': 'PASS'}, {'id': 'b', 'status': 'FAIL'}, {'id': 'c', 'status': 'PASS'}]}))
            self.assertEqual(release_run.summary(path), (2, ['b']))


if __name__ == '__main__':
    unittest.main()
