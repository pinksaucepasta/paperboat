#!/usr/bin/env python3
"""S3-04: run the full device smoke suite on an OFFICIAL release, Linux pair (hp + dadape).

  release_run.py --version 2026.10.19.17 --installer-url https://<get host>/install [--dry-run]
                 [-- smoke.sh options, e.g. --long-seconds 60]

1. Reads the published one-liner and requires it to pin exactly --version for linux-amd64 and
   linux-arm64 (the release users get), then downloads those binaries from the pinned URLs and
   verifies length and SHA-256 the way the installer does.
2. Installs each binary on its device with deploy.py (hp amd64, dadape arm64): the same `pb install`
   a user runs, no identity or account change.
3. Runs smoke.sh --targets hp,dadape --expect-version VERSION (all checks, no skip list) and prints
   the result file and a CHECKLIST evidence line.
Needs DADAPE_PASS (REMOTE_TESTING.md) and a signed-in HP/dadape pair. --dry-run stops after step 1.
"""
import argparse
import hashlib
import json
import os
import re
import subprocess
import sys
import tempfile
import urllib.parse
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
DEVICES = {'hp': ('linux-amd64', 'anvit@hp'), 'dadape': ('linux-arm64', 'anvit@dadape')}
MAX_ASSET = 512 << 20
BLOCK = re.compile(r"^\s*(linux-(?:amd64|arm64))\)\s*\n\s*product_version='([^']+)'\s*\n\s*product_url='([^']+)'\s*\n"
                   r"\s*product_sha='([0-9a-f]{64})'\s*\n\s*product_length='([0-9]+)'", re.M)


def fetch(url, limit):
    parsed = urllib.parse.urlsplit(url)
    if parsed.scheme != 'https':
        raise SystemExit('refusing a non-https URL: ' + url)
    with urllib.request.urlopen(url, timeout=120) as response:
        data = response.read(limit + 1)
    if len(data) > limit:
        raise SystemExit('download exceeds its size bound: ' + url)
    return data


def pins(installer_text):
    found = {name: dict(version=v, url=u, sha256=s, length=int(n)) for name, v, u, s, n in BLOCK.findall(installer_text)}
    if set(found) != {'linux-amd64', 'linux-arm64'}:
        raise SystemExit('the published installer does not pin both Linux binaries')
    return found


def download(version, installer_url, directory):
    pinned = pins(fetch(installer_url, 1 << 20).decode())
    files = {}
    for name, pin in sorted(pinned.items()):
        if pin['version'] != version:
            raise SystemExit(f"the one-liner pins {pin['version']} for {name}, not {version}; run the suite on what users get")
        if not 0 < pin['length'] <= MAX_ASSET or not pin['url'].startswith('https://'):
            raise SystemExit('invalid pin for ' + name)
        data = fetch(pin['url'], pin['length'])
        if len(data) != pin['length'] or hashlib.sha256(data).hexdigest() != pin['sha256']:
            raise SystemExit(f'{name} does not match the published length and SHA-256')
        path = directory / ('pb-' + name)
        path.write_bytes(data)
        path.chmod(0o700)
        files[name] = path
    return files


def summary(result_file):
    rows = json.loads(result_file.read_text()).get('results', [])
    failed = [row['id'] for row in rows if row.get('status') == 'FAIL']
    passed = sum(1 for row in rows if row.get('status') == 'PASS')
    return passed, failed


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--version', required=True)
    parser.add_argument('--installer-url', default=os.environ.get('PAPERBOAT_INSTALLER_URL', ''))
    parser.add_argument('--dry-run', action='store_true')
    parser.add_argument('smoke', nargs=argparse.REMAINDER, help='options passed to smoke.sh after --')
    args = parser.parse_args()
    if not args.installer_url:
        raise SystemExit('--installer-url or PAPERBOAT_INSTALLER_URL is required')
    extra = args.smoke[1:] if args.smoke[:1] == ['--'] else args.smoke
    if '--skip' in extra or '--only-check' in extra or '--only-list' in extra:
        raise SystemExit('the release run executes every check; do not skip or select checks')
    with tempfile.TemporaryDirectory(prefix='pb-release-run-') as directory:
        files = download(args.version, args.installer_url, Path(directory))
        print(json.dumps({'phase': 'verified', 'version': args.version, 'assets': sorted(files)}), flush=True)
        if args.dry_run:
            return 0
        password = os.environ.get('DADAPE_PASS', '')
        if not password:
            raise SystemExit('export DADAPE_PASS (see REMOTE_TESTING.md)')
        for device, (asset, target) in DEVICES.items():
            env = dict(os.environ, DEVICE_PASSWORD=password) if device == 'dadape' else dict(os.environ)
            receipts = Path(directory) / ('receipts-' + device)
            subprocess.run([sys.executable, str(HERE / 'deploy.py'), '--device', device, '--ssh-target', target,
                            '--binary', str(files[asset]), '--version', args.version, '--receipts', str(receipts)],
                           check=True, env=env)
    results = HERE / 'results'
    before = set(results.glob('*.json'))
    status = subprocess.run([str(HERE / 'smoke.sh'), '--targets', 'hp,dadape', '--expect-version', args.version, *extra]).returncode
    new = sorted(set(results.glob('*.json')) - before)
    if not new:
        raise SystemExit('smoke.sh wrote no result file')
    passed, failed = summary(new[-1])
    print(json.dumps({'phase': 'result', 'file': str(new[-1].relative_to(HERE.parent.parent)), 'version': args.version,
                      'passed': passed, 'failed': failed,
                      'evidence': f"official {args.version} on hp+dadape: smoke {new[-1].name}, {passed} PASS, {len(failed)} FAIL"}))
    return status or (1 if failed else 0)


if __name__ == '__main__':
    sys.exit(main())
