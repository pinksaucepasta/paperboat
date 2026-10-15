import hashlib
import json
import lzma
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest

import deploy

HERE = Path(__file__).resolve().parent


class DeviceDeployment(unittest.TestCase):
    def test_verified_compressed_candidate_and_failures_clean_staging(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            marker = root / 'installed'
            # A real executable at the remote installer boundary, not a mocked receipt.
            binary = ('#!/usr/bin/env python3\nimport json,sys\nfrom pathlib import Path\n'
                      'if "--version" in sys.argv: print(json.dumps({"ok":True,"data":{"version":"dev-fixture"}}))\n'
                      'else:\n Path(' + repr(str(marker)) + ').write_text("installed")\n'
                      ' print(json.dumps({"ok":True,"data":{"executable":"fixture"}}))\n').encode()
            for failure in ['checksum', 'version', None]:
                with self.subTest(failure=failure):
                    archive = root / 'candidate.xz'
                    archive.write_bytes(lzma.compress(binary))
                    request = dict(archive=str(archive), sha256=hashlib.sha256(binary).hexdigest(),
                                   version='dev-fixture', install_timeout=3)
                    if failure == 'checksum':
                        request['sha256'] = '0' * 64
                    if failure == 'version':
                        request['version'] = 'dev-wrong'
                    result = subprocess.run([sys.executable, str(HERE / 'device_install.py')],
                                            input=json.dumps(request), text=True, capture_output=True)
                    self.assertEqual(result.returncode == 0, failure is None)
                    self.assertFalse(archive.exists(), 'compressed staging must be removed on every exit')
                    self.assertEqual(marker.exists(), failure is None, 'unverified bytes must never install')
            marker.unlink()
            failing = binary.replace(b'"ok":True,"data":{"executable":"fixture"}',
                                     b'"ok":False,"error":{"code":"install_failed"}')
            archive.write_bytes(lzma.compress(failing))
            request.update(sha256=hashlib.sha256(failing).hexdigest(), version='dev-fixture')
            result = subprocess.run([sys.executable, str(HERE / 'device_install.py')],
                                    input=json.dumps(request), text=True, capture_output=True)
            self.assertNotEqual(0, result.returncode)
            self.assertIn('install_failed', result.stdout, 'primary installer receipt must survive failure')
            self.assertFalse(archive.exists())

    def test_transport_timeout_reaps_child_and_keeps_output_private(self):
        with tempfile.TemporaryDirectory() as directory:
            receipt = Path(directory) / 'receipt'
            old = os.umask(0o077)
            try:
                started = time.monotonic()
                with self.assertRaises(subprocess.TimeoutExpired):
                    deploy.run([sys.executable, '-c',
                                'import os,time;print(os.getpid(),flush=True);time.sleep(60)'],
                               timeout=0.2, receipt=receipt)
                self.assertLess(time.monotonic() - started, 3)
                with self.assertRaises(ProcessLookupError):
                    os.kill(int(receipt.read_text()), 0)
                self.assertEqual(0o600, receipt.stat().st_mode & 0o777)
            finally:
                os.umask(old)


if __name__ == '__main__':
    unittest.main()
