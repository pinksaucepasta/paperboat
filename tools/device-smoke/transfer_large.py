#!/usr/bin/env python3
"""Large-transfer check: one file well over a 64 MiB segment (default 1536 MiB, under the 2 GB Automatic bound) and
a folder tree with an empty directory, each `pb send --to TARGET`, timed and verified byte for byte on the receiver.

Prints one RESULT line per case (MB/s) and one JSON receipt as the last line, like the other transfer checks, and
writes results/transfer-large-<UTC>.json. --netem-ms adds delay on this machine's egress (sudo -n tc netem) for the
run. --pb sends with another pb binary (an older build) for a before/after comparison against the same receiver.
Unix only: the Windows form of the verification belongs with windows_target.
"""
import argparse
import json
import pathlib
import subprocess
import time
import uuid
from datetime import datetime, timezone

from exec_lifecycle import run
from install_lifecycle import registration
from pairing_lifecycle import invoke
import winhost

CHECK = 'transfer-large'
AUTOMATIC_LIMIT = 2_000_000_000
SEGMENT = 64 << 20
# (relative path, MiB): two files over a segment in different directories, small files, and an empty directory.
TREE = [('a/big-0.bin', 80), ('a/big-1.bin', 80), ('b/mid.bin', 30), ('b/small-0.bin', 1), ('b/small-1.bin', 1)]
EMPTY = 'empty-keep'


def sh(*command):
    return subprocess.run(command, capture_output=True, text=True, timeout=120)


class Netem:
    def __init__(self, delay_ms):
        self.delay_ms, self.interface = delay_ms, None

    def __enter__(self):
        if self.delay_ms:
            self.interface = sh('sh', '-c', "ip -o route get 1.1.1.1 | sed -n 's/.* dev \\([^ ]*\\).*/\\1/p'").stdout.strip()
            assert self.interface, 'no default interface'
            done = sh('sudo', '-n', 'tc', 'qdisc', 'replace', 'dev', self.interface, 'root', 'netem', 'delay',
                      '%dms' % self.delay_ms, 'limit', '100000')
            assert done.returncode == 0, 'netem failed: ' + done.stderr[-200:]
        return self

    def __exit__(self, *_):
        if self.interface:
            sh('sudo', '-n', 'tc', 'qdisc', 'del', 'dev', self.interface, 'root')


def send_timed(binary, source, target):
    start = time.monotonic()
    result = invoke(binary, 'send', str(source), '--to', target, '--json', timeout=7200)
    return time.monotonic() - start, result


def on_receiver(binary, target, script):
    output, errors = run(binary, 'exec', target, '--', 'python3', '-c', script)
    if errors or not output.strip().endswith(b'VERIFIED'):
        raise RuntimeError('receiver verification failed: ' + (output + errors).decode(errors='replace')[-300:])


VERIFY = '''
import hashlib, sys
from pathlib import Path
root = Path.home() / "Paperboat" / "Inbox"
expected = %r
for rel, (size, digest) in expected.items():
    path = root / rel
    h = hashlib.sha256()
    with path.open("rb") as handle:
        for block in iter(lambda: handle.read(1 << 20), b""):
            h.update(block)
    assert path.stat().st_size == size and h.hexdigest() == digest, rel
for rel in %r:
    assert (root / rel).is_dir(), rel
print("VERIFIED")
'''

CLEAN = '''
from pathlib import Path
root = Path.home() / "Paperboat" / "Inbox"
for name in %r:
    path = root / name
    shutil.rmtree(path) if path.is_dir() else path.unlink(missing_ok=True)
'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--mib', type=int, default=1536)
    parser.add_argument('--netem-ms', type=int, default=0)
    parser.add_argument('--pb', default=None)
    args = parser.parse_args()
    size = args.mib << 20
    if not SEGMENT < size < AUTOMATIC_LIMIT:
        parser.error('the file must exceed one 64 MiB segment and stay under the 2 GB Automatic bound')
    if winhost.device():
        parser.error('Unix only')
    binary = pathlib.Path(args.pb) if args.pb else winhost.pb_path()
    before = registration()
    name = 'paperboat-smoke-large-' + uuid.uuid4().hex
    cases = []
    with winhost.workdir(name) as directory:
        big = directory / (name + '.bin')
        big_digest = winhost.write_random(big, size)
        tree = directory / (name + '-tree')
        files = {}
        for rel, mib in TREE:
            path = tree / rel
            path.parent.mkdir(parents=True, exist_ok=True)
            files[name + '-tree/' + rel] = (mib << 20, winhost.write_random(path, mib << 20))
        (tree / EMPTY).mkdir()
        tree_bytes = sum(mib << 20 for _, mib in TREE)
        try:
            with Netem(args.netem_ms):
                seconds, _ = send_timed(binary, big, args.target)
                print('RESULT case=file mib=%d seconds=%.1f MBps=%.2f netem_ms=%d' % (args.mib, seconds, size / seconds / 1e6, args.netem_ms), flush=True)
                on_receiver(binary, args.target, VERIFY % ({big.name: (size, big_digest)}, []))
                cases.append({'case': 'file', 'bytes': size, 'seconds': round(seconds, 1), 'MBps': round(size / seconds / 1e6, 2), 'sha256': big_digest})
                seconds, _ = send_timed(binary, tree, args.target)
                print('RESULT case=folder bytes=%d files=%d seconds=%.1f MBps=%.2f netem_ms=%d' % (tree_bytes, len(TREE), seconds, tree_bytes / seconds / 1e6, args.netem_ms), flush=True)
                on_receiver(binary, args.target, VERIFY % (files, [name + '-tree/' + EMPTY]))
                cases.append({'case': 'folder', 'bytes': tree_bytes, 'files': len(TREE), 'seconds': round(seconds, 1), 'MBps': round(tree_bytes / seconds / 1e6, 2)})
        finally:
            try:
                run(binary, 'exec', args.target, '--', 'python3', '-c', CLEAN % ([big.name, tree.name],))
            except Exception as error:  # the verification result matters more than a leftover test file
                print('cleanup failed:', error, flush=True)
    if registration() != before:
        raise RuntimeError('transfer large changed the enrolled identity')
    receipt = {'check': CHECK, 'target': args.target, 'status': 'PASS', 'pb': str(binary), 'netem_ms': args.netem_ms,
               'cases': cases, 'enrollment_unchanged': True}
    results = pathlib.Path(__file__).resolve().parent / 'results'
    results.mkdir(exist_ok=True)
    stamp = datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')
    (results / ('transfer-large-%s.json' % stamp)).write_text(json.dumps(receipt, indent=1))
    print(json.dumps(receipt))


if __name__ == '__main__':
    main()
