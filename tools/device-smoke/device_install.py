#!/usr/bin/env python3
"""Single remote install transaction for both launch devices; JSON on stdin."""
import hashlib
import json
import lzma
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile

import terminal_command


def run_installer(candidate, request):
    arguments = [str(candidate), 'install', '--json']
    if request.get('password') and sys.platform != 'darwin':
        result = terminal_command.run(arguments, password=request['password'],
                                      timeout=request['install_timeout'])
        # sudo's prompt shares the PTY with stdout. Preserve that diagnostic,
        # but emit the actual installer's JSON receipt separately.
        sys.stderr.write(result.stdout)
        receipt = None
        for line in result.stdout.splitlines():
            try:
                value = json.loads(line[line.index('{'):])
                if isinstance(value, dict) and isinstance(value.get('ok'), bool):
                    receipt = value
            except (ValueError, TypeError):
                continue
        if receipt is None:
            raise RuntimeError('supplied installer did not return its JSON receipt')
        result.stdout = json.dumps(receipt)
        return result
    return subprocess.run(arguments, capture_output=True, text=True,
                          timeout=request['install_timeout'])


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
            result = run_installer(candidate, request)
            # Preserve the complete supplied-installer receipt privately at the caller.
            sys.stdout.write(result.stdout)
            sys.stderr.write(result.stderr)
            if result.returncode or json.loads(result.stdout).get('ok') is not True:
                raise RuntimeError('supplied candidate installation failed')
    finally:
        archive.unlink(missing_ok=True)


if __name__ == '__main__':
    def disconnected(signum, frame):
        # subprocess.run kills/reaps its child when interrupted; then the
        # transaction's finally blocks remove both decompressed and uploaded bytes.
        raise RuntimeError('remote install transport disconnected')

    for signum in (signal.SIGHUP, signal.SIGTERM):
        signal.signal(signum, disconnected)
    install(json.load(sys.stdin))
