import hashlib
import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import release_run


def installer(version, amd64, arm64):
    block = lambda name, data: (f"  {name})\n    product_version='{version}'\n"
                                f"    product_url='https://example.test/{name}'\n"
                                f"    product_sha='{hashlib.sha256(data).hexdigest()}'\n"
                                f"    product_length='{len(data)}' ;;\n")
    return "case x in\n" + block('linux-amd64', amd64) + block('linux-arm64', arm64) + "esac\n"


class ReleaseRunTest(unittest.TestCase):
    def serve(self, text, assets):
        def fetch(url, limit):
            return text.encode() if url.endswith('/install') else assets[url.rsplit('/', 1)[1]]
        return mock.patch.object(release_run, 'fetch', fetch)

    def test_downloads_exactly_the_pinned_release(self):
        assets = {'linux-amd64': b'amd64 bytes', 'linux-arm64': b'arm64 bytes'}
        with self.serve(installer('2026.10.19.17', *assets.values()), assets), tempfile.TemporaryDirectory() as d:
            files = release_run.download('2026.10.19.17', 'https://example.test/install', Path(d))
            self.assertEqual(files['linux-arm64'].read_bytes(), b'arm64 bytes')

    def test_refuses_a_release_the_one_liner_does_not_serve(self):
        assets = {'linux-amd64': b'a', 'linux-arm64': b'b'}
        with self.serve(installer('2026.10.19.18', *assets.values()), assets), tempfile.TemporaryDirectory() as d:
            with self.assertRaisesRegex(SystemExit, 'not 2026.10.19.17'):
                release_run.download('2026.10.19.17', 'https://example.test/install', Path(d))

    def test_refuses_bytes_that_do_not_match_the_pin(self):
        with self.serve(installer('2026.10.19.17', b'good', b'good'), {'linux-amd64': b'evil', 'linux-arm64': b'good'}), \
                tempfile.TemporaryDirectory() as d:
            with self.assertRaisesRegex(SystemExit, 'SHA-256'):
                release_run.download('2026.10.19.17', 'https://example.test/install', Path(d))

    def test_refuses_plain_http_and_a_partial_installer(self):
        with self.assertRaisesRegex(SystemExit, 'non-https'):
            release_run.fetch('http://example.test/install', 10)
        with self.assertRaisesRegex(SystemExit, 'both Linux'):
            release_run.pins("case x in\n  linux-amd64)\n    product_version='1'\n    product_url='https://e'\n    product_sha='" + 'a' * 64 + "'\n    product_length='1' ;;\nesac\n")

    def test_the_run_executes_every_check(self):
        for flag in ('--skip', '--only-check', '--only-list'):
            with self.subTest(flag=flag), mock.patch.object(sys, 'argv', ['x', '--version', '1', '--installer-url', 'https://e/install', '--', flag, 'a']):
                with self.assertRaisesRegex(SystemExit, 'every check'):
                    release_run.main()

    def test_summary_counts_results(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / 'r.json'
            path.write_text(json.dumps({'results': [{'id': 'a', 'status': 'PASS'}, {'id': 'b', 'status': 'FAIL'}, {'id': 'c', 'status': 'PASS'}]}))
            self.assertEqual(release_run.summary(path), (2, ['b']))


if __name__ == '__main__':
    unittest.main()
