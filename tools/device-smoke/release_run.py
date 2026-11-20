#!/usr/bin/env python3
"""S3-04: run the full device smoke suite on an OFFICIAL release, configured Linux pair.

  release_run.py --version 2026.10.19.17 [--repository owner/repo] [--dry-run]
                 [-- smoke.sh options, e.g. --long-seconds 60]

1. Downloads the release's SHA256SUMS and SHA256SUMS.sig from GitHub, verifies the signature with the release key
   embedded in cli/tools/install.sh (the key the installer trusts), requires it to name exactly --version, then
   downloads pb-linux-amd64 and pb-linux-arm64 and verifies their SHA-256 the way the installer does.
2. Installs each binary on its device with deploy.py (one amd64 and one arm64 host): the same `pb install`
   a user runs, no identity or account change.
3. Runs smoke.sh --targets <linux>,<peer> --expect-version VERSION (all checks, no skip list) and prints
   the result file and a CHECKLIST evidence line.
Use a signed-in Linux pair from the local target configuration. --dry-run stops after step 1.
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
from dev_targets import configured_path, load, smoke_environment
MAX_ASSET = 512 << 20
INSTALLER = HERE.parents[1] / 'cli' / 'tools' / 'install.sh'
RELEASE_KEY = re.compile(r'paperboat-release namespaces="paperboat-release" ssh-ed25519 [A-Za-z0-9+/=]+')


def fetch(url, limit):
    parsed = urllib.parse.urlsplit(url)
    if parsed.scheme != 'https':
        raise SystemExit('refusing a non-https URL: ' + url)
    with urllib.request.urlopen(url, timeout=120) as response:
        data = response.read(limit + 1)
    if len(data) > limit:
        raise SystemExit('download exceeds its size bound: ' + url)
    return data


def verified_sums(version, base):
    """The release's SHA256SUMS as {asset: sha256}, only after the signature verifies with the installer's embedded key."""
    sums, signature = fetch(base + '/SHA256SUMS', 1 << 20), fetch(base + '/SHA256SUMS.sig', 1 << 16)
    key = RELEASE_KEY.search(INSTALLER.read_text())
    if not key:
        raise SystemExit('cli/tools/install.sh embeds no release key')
    with tempfile.TemporaryDirectory(prefix='pb-release-sig-') as directory:
        allowed, sig = Path(directory) / 'allowed_signers', Path(directory) / 'SHA256SUMS.sig'
        allowed.write_text(key.group(0) + '\n')
        sig.write_bytes(signature)
        check = subprocess.run(['ssh-keygen', '-Y', 'verify', '-f', str(allowed), '-I', 'paperboat-release', '-n', 'paperboat-release', '-s', str(sig)],
                               input=sums, capture_output=True)
    if check.returncode != 0:
        raise SystemExit('the release signature on SHA256SUMS is not valid')
    lines = sums.decode().splitlines()
    if not lines or lines[0] != f'# paperboat-release {version}':
        raise SystemExit(f'SHA256SUMS is not signed for {version}; run the suite on what users get')
    found = {}
    for line in lines[1:]:
        digest, _, name = line.partition('  ')
        if re.fullmatch(r'[0-9a-f]{64}', digest) and name:
            found[name] = digest
    return found


def download(version, repository, directory):
    base = f'https://github.com/{repository}/releases/download/{version}'
    sums = verified_sums(version, base)
    files = {}
    for name in ('linux-amd64', 'linux-arm64'):
        asset = 'pb-' + name
        if asset not in sums:
            raise SystemExit(f'SHA256SUMS does not list {asset}')
        data = fetch(f'{base}/{asset}', MAX_ASSET)
        if hashlib.sha256(data).hexdigest() != sums[asset]:
            raise SystemExit(f'{name} does not match the signed SHA-256')
        path = directory / asset
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
    parser.add_argument('--repository', default=os.environ.get('PB_REPO', 'pinksaucepasta/paperboat'), help='owner/repo that publishes the releases')
    parser.add_argument('--dry-run', action='store_true')
    parser.add_argument('smoke', nargs=argparse.REMAINDER, help='options passed to smoke.sh after --')
    args = parser.parse_args()
    if not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', args.repository):
        raise SystemExit('--repository must look like owner/repo')
    extra = args.smoke[1:] if args.smoke[:1] == ['--'] else args.smoke
    if '--skip' in extra or '--only-check' in extra or '--only-list' in extra:
        raise SystemExit('the release run executes every check; do not skip or select checks')
    with tempfile.TemporaryDirectory(prefix='pb-release-run-') as directory:
        files = download(args.version, args.repository, Path(directory))
        print(json.dumps({'phase': 'verified', 'version': args.version, 'assets': sorted(files)}), flush=True)
        if args.dry_run:
            return 0
        settings = load()
        smoke = smoke_environment(settings)
        devices = [(smoke.get('SMOKE_LINUX_NAME', 'linux'), smoke.get('SMOKE_LINUX_SSH'), 'linux-amd64', ''),
                   (smoke.get('SMOKE_PEER_NAME', 'linux-peer'), smoke.get('SMOKE_LINUX_PEER_SSH'), 'linux-arm64',
                    smoke.get('SMOKE_LINUX_PEER_PASSWORD', ''))]
        if not all(target for _, target, _, _ in devices):
            parser.error('configure both Linux SSH logins in the local development targets')
        for device, target, asset, password in devices:
            env = dict(os.environ, DEVICE_PASSWORD=password)
            receipts = Path(directory) / ('receipts-' + device)
            subprocess.run([sys.executable, str(HERE / 'deploy.py'), '--device', device, '--ssh-target', target,
                            '--binary', str(files[asset]), '--version', args.version, '--receipts', str(receipts)],
                           check=True, env=env)
    results = configured_path(settings, 'results_dir', HERE / 'results')
    targets = ','.join(device for device, *_ in devices)
    before = set(results.glob('*.json'))
    status = subprocess.run([str(HERE / 'smoke.sh'), '--targets', targets, '--expect-version', args.version, *extra]).returncode
    new = sorted(set(results.glob('*.json')) - before)
    if not new:
        raise SystemExit('smoke.sh wrote no result file')
    passed, failed = summary(new[-1])
    print(json.dumps({'phase': 'result', 'file': str(new[-1]), 'version': args.version,
                      'passed': passed, 'failed': failed,
                      'evidence': f"official {args.version} on the configured Linux pair: smoke {new[-1].name}, {passed} PASS, {len(failed)} FAIL"}))
    return status or (1 if failed else 0)


if __name__ == '__main__':
    sys.exit(main())
