#!/usr/bin/env python3
"""Single remote install transaction for both launch devices; JSON on stdin."""
import hashlib
import json
import lzma
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile


def install(request):
    archive = Path(request['archive'])
    try:
        with tempfile.TemporaryDirectory(prefix='paperboat-install-') as directory:
            candidate = Path(directory) / 'pb'
            with archive.open('rb') as compressed, lzma.open(compressed) as source:
                with candidate.open('xb') as target:
                    shutil.copyfileobj(source, target)
            if hashlib.sha256(candidate.read_bytes()).hexdigest() != request['sha256']:
                raise RuntimeError('candidate checksum differs; installation refused')
            candidate.chmod(0o700)
            version = subprocess.run([str(candidate), '--version', '--json'],
                                     check=True, capture_output=True, text=True, timeout=30)
            actual = json.loads(version.stdout)
            if actual.get('data', actual).get('version') != request['version']:
                raise RuntimeError('candidate version differs; installation refused')
            if sys.platform == 'darwin':
                subprocess.run(['security', 'unlock-keychain', '-p', request['password'],
                                str(Path.home() / 'Library/Keychains/login.keychain-db')],
                               check=True, capture_output=True, timeout=30)
            result = subprocess.run([str(candidate), 'install', '--json'],
                                    capture_output=True, text=True, timeout=request['install_timeout'])
            # Preserve the complete supplied-installer receipt privately at the caller.
            sys.stdout.write(result.stdout)
            sys.stderr.write(result.stderr)
            if result.returncode or json.loads(result.stdout).get('ok') is not True:
                raise RuntimeError('supplied candidate installation failed')
    finally:
        archive.unlink(missing_ok=True)


if __name__ == '__main__':
    install(json.load(sys.stdin))
