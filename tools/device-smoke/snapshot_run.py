#!/usr/bin/env python3
"""Freeze the shell program and device fixtures for one source window."""
import os
from pathlib import Path
import subprocess
import sys
import tempfile


def main():
    script = Path(sys.argv[1]).resolve()
    with tempfile.TemporaryDirectory(prefix='paperboat-smoke-source-') as directory:
        frozen = Path(directory) / 'smoke.sh'
        frozen.write_bytes(script.read_bytes())
        archive = Path(directory) / 'fixtures.tar.gz'
        with archive.open('wb') as output:
            subprocess.run([sys.executable, str(Path(__file__).with_name('stage_fixtures.py')),
                            str(script.parent)], stdout=output, check=True)
        environment = dict(os.environ, PAPERBOAT_SMOKE_SNAPSHOT='1',
                           PAPERBOAT_SMOKE_SOURCE_DIR=str(script.parent),
                           PAPERBOAT_SMOKE_FIXTURES=str(archive))
        return subprocess.run(['bash', str(frozen), *sys.argv[2:]], env=environment).returncode


if __name__ == '__main__':
    raise SystemExit(main())
