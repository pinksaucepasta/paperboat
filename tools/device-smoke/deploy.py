#!/usr/bin/env python3
"""Deploy a committed device binary using XZ, verified bytes and one remote installer.

Example: deploy.py --device hp --ssh-target user@host --binary .build/dev-HASH/pb-linux-amd64
  --version dev-HASH --receipts /private/path
For Apple, supply DEVICE_PASSWORD in the environment (sshpass must be installed).
Upload budgets scale with compressed size at 128 KiB/s plus connection/setup slack.
No enrollment, credentials, OS approval policies or existing identity are changed.
"""
import argparse
import hashlib
import json
import lzma
import math
import os
from pathlib import Path
import shlex
import signal
import shutil
import subprocess
import tempfile
import uuid

HERE = Path(__file__).resolve().parent


def run(argv, *, timeout, receipt, env=None, payload=None):
    """Keep output private and kill/reap the complete transport group on timeout."""
    with receipt.open('wb') as output, receipt.with_suffix('.stderr').open('wb') as error:
        process = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=output, stderr=error,
                                   env=env, start_new_session=True)
        try:
            process.communicate(payload, timeout=timeout)
        except BaseException:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait()
            raise
        if process.returncode:
            raise RuntimeError('device step failed; private receipt: ' + str(receipt))


def deploy(args):
    os.umask(0o077)
    args.receipts.mkdir(parents=True, exist_ok=True, mode=0o700)
    prefix = []
    env = dict(os.environ)
    if args.device == 'apple':
        password = env.pop('DEVICE_PASSWORD')
        env['SSHPASS'] = password
        prefix = ['sshpass', '-e']
    else:
        password = ''
    digest = hashlib.sha256(args.binary.read_bytes()).hexdigest()
    remote = '/tmp/paperboat-candidate-' + uuid.uuid4().hex + '.xz'
    options = ['-o', 'ConnectTimeout=15', '-o', 'ServerAliveInterval=15',
               '-o', 'ServerAliveCountMax=4']
    with tempfile.TemporaryDirectory(prefix='paperboat-upload-') as directory:
        archive = Path(str(args.binary) + '.xz')
        if not archive.exists():
            archive = Path(directory) / 'pb.xz'
            with args.binary.open('rb') as source, lzma.open(archive, 'wb', preset=3) as target:
                shutil.copyfileobj(source, target)
        budget = 60 + max(120, math.ceil(archive.stat().st_size / (128 * 1024)))
        print(json.dumps({'phase': 'upload', 'device': args.device,
                          'bytes': archive.stat().st_size, 'budget_seconds': budget}), flush=True)
        try:
            run(prefix + ['scp', *options, str(archive), args.ssh_target + ':' + remote],
                timeout=budget, receipt=args.receipts / 'upload.log', env=env)
            request = dict(archive=remote, sha256=digest, version=args.version,
                           password=password, install_timeout=args.install_timeout)
            command = 'python3 -c ' + shlex.quote((HERE / 'device_install.py').read_text())
            print(json.dumps({'phase': 'install', 'device': args.device}), flush=True)
            receipt = args.receipts / 'install.json'
            run(prefix + ['ssh', *options, args.ssh_target, command],
                timeout=args.install_timeout + 90, receipt=receipt, env=env,
                payload=json.dumps(request).encode())
            if json.loads(receipt.read_text()).get('ok') is not True:
                raise RuntimeError('invalid install receipt: ' + str(receipt))
            print(json.dumps({'phase': 'installed', 'device': args.device,
                              'version': args.version, 'sha256_verified': True}), flush=True)
        finally:
            # Also remove partial uploads when SCP or the remote interpreter fails.
            import sys
            primary_failure = sys.exc_info()[0] is not None
            try:
                run(prefix + ['ssh', *options, args.ssh_target, 'rm -f -- ' + shlex.quote(remote)],
                    timeout=75, receipt=args.receipts / 'cleanup.log', env=env)
            except Exception:
                if not primary_failure:
                    raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--device', required=True, choices=['hp', 'apple'])
    parser.add_argument('--ssh-target', required=True)
    parser.add_argument('--binary', required=True, type=Path)
    parser.add_argument('--version', required=True)
    parser.add_argument('--receipts', required=True, type=Path)
    parser.add_argument('--install-timeout', type=int, default=300)
    deploy(parser.parse_args())


if __name__ == '__main__':
    main()
