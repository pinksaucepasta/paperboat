#!/usr/bin/env python3
"""Build all launch device artifacts once from an isolated committed revision.

build_devices.py --server-url CONFIGURED_API_URL --output .build [--revision HEAD]
Custom test builds deliberately have no release URL and cannot auto-update.
The manifest appears only after all three binaries and compressed archives succeed.
"""
import argparse
import hashlib
import json
import lzma
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[2]


def git(*args):
    return subprocess.check_output(['git', '-C', str(ROOT), *args], text=True).strip()


def build(args):
    commit = git('rev-parse', args.revision + '^{commit}')
    version = 'dev-' + git('rev-parse', '--short', commit)
    destination = args.output.resolve() / version
    destination.mkdir(parents=True, exist_ok=True)
    manifest = destination / 'manifest.json'
    if manifest.exists():
        raise RuntimeError('committed build already exists; use its manifest instead of rebuilding')
    values = dict(Version=version, Commit=commit, ProtocolVersion='1',
                  DefaultServerURL=args.server_url, DefaultReleaseURL='')
    flags = ' '.join('-X github.com/pinksaucepasta/paperboat/internal/buildinfo.' + key + '=' + value
                     for key, value in values.items())
    with tempfile.TemporaryDirectory(prefix='paperboat-device-source-') as directory:
        source = Path(directory) / 'source'
        subprocess.run(['git', '-C', str(ROOT), 'worktree', 'add', '--detach', str(source), commit],
                       check=True, stdout=subprocess.DEVNULL)
        artifacts = {}
        try:
            for platform, architecture in [('linux', 'amd64'), ('linux', 'arm64'), ('darwin', 'arm64')]:
                name = 'pb-' + platform + '-' + architecture
                binary = destination / name
                subprocess.run(['go', 'build', '-trimpath', '-ldflags', flags, '-o', str(binary), './cmd/pb'],
                               cwd=source / 'cli', check=True,
                               env=dict(os.environ, CGO_ENABLED='0', GOOS=platform, GOARCH=architecture))
                with binary.open('rb') as raw, lzma.open(str(binary) + '.xz', 'wb', preset=3) as compressed:
                    shutil.copyfileobj(raw, compressed)
                artifacts[name] = dict(sha256=hashlib.sha256(binary.read_bytes()).hexdigest(),
                                       bytes=binary.stat().st_size)
                print(version, name, 'PASS', flush=True)
            temporary = destination / 'manifest.pending'
            temporary.write_text(json.dumps(dict(commit=commit, version=version, artifacts=artifacts)))
            temporary.replace(manifest)
        finally:
            subprocess.run(['git', '-C', str(ROOT), 'worktree', 'remove', '--force', str(source)], check=True)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--revision', default='HEAD')
    parser.add_argument('--server-url', required=True)
    parser.add_argument('--output', type=Path, required=True)
    build(parser.parse_args())
