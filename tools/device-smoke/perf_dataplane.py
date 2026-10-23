#!/usr/bin/env python3
"""Data-plane performance from this machine to a target, checked against perf_budgets.

Measures `pb exec true` latency, pb send / managed scp throughput of a 50 MiB file,
managed rsync of many small files, the selected path, and how fast `pb --version` and
`pb status` answer locally, then compares each number with the budget for that path. Prints
one JSON receipt (numbers only; no file or command contents). Usage: perf_dataplane.py <target> [--no-budget]
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import statistics
import subprocess
import tempfile
import time
import uuid

from install_lifecycle import registration
from perf_budgets import BUDGETS

PB = Path.home() / '.local/bin/pb'
LARGE_BYTES = 50 << 20
SMALL_FILES, SMALL_BYTES = 500, 4096


def pb(*arguments, timeout=600):
    """Run pb, return elapsed seconds; output stays private and bounded."""
    started = time.monotonic()
    with tempfile.TemporaryFile() as output:
        result = subprocess.run([str(PB), *arguments], stdout=output, stderr=output,
                                stdin=subprocess.DEVNULL, timeout=timeout)
    if result.returncode != 0:
        raise RuntimeError(f'pb {arguments[0]} exited {result.returncode}')
    return time.monotonic() - started


def remote(target, script, timeout=120):
    """Run a short python3 script on the target and return its stdout (expected to be small)."""
    result = subprocess.run([str(PB), 'exec', target, '--', 'python3', '-c', script], capture_output=True,
                            text=True, stdin=subprocess.DEVNULL, timeout=timeout)
    if result.returncode != 0 or len(result.stdout) > 4096:
        raise RuntimeError('remote helper failed')
    return result.stdout.strip()


def selected_path(target):
    """The path the peer transport uses now, from an authenticated ping (pb status shows only
    paths in use by a consumer, so it reads 'none' between transfers)."""
    result = subprocess.run([str(PB), 'ping', target, '--count', '2', '--json'], capture_output=True,
                            text=True, stdin=subprocess.DEVNULL, timeout=120)
    samples = [s for s in json.loads(result.stdout).get('samples') or [] if not s.get('lost')]
    return samples[-1].get('path') or 'none' if samples else 'none'


def local_command_ms(*arguments, runs=7):
    """Median wall time of a local pb command that must not wait on the network."""
    pb(*arguments, timeout=60)  # page the binary in
    return round(statistics.median(pb(*arguments, timeout=60) for _ in range(runs)) * 1000)


def exec_latency(target, runs=9):
    pb('exec', target, '--', 'true', timeout=120)  # warm the session and path
    samples = sorted(pb('exec', target, '--', 'true', timeout=120) for _ in range(runs))
    return round(statistics.median(samples) * 1000), round(samples[-1] * 1000)


def find_and_hash(target, name):
    return remote(target, (
        'import hashlib,pathlib\n'
        f'm=[p for p in pathlib.Path.home().rglob({name!r}) if "/tmp/" not in str(p)][:1]\n'
        'print(hashlib.sha256(m[0].read_bytes()).hexdigest() if m else "")\n'
        'm and m[0].unlink()\n'))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--no-budget', action='store_true', help='measure only')
    args = parser.parse_args()
    before = registration()
    run = 'perf-' + uuid.uuid4().hex[:12]
    # Local commands first: right after network work the daemon is still busy with that session.
    metrics = {'cli_start_ms': local_command_ms('--version'), 'status_ms': local_command_ms('status')}
    metrics['path'] = selected_path(args.target)
    metrics['exec_true_median_ms'], metrics['exec_true_max_ms'] = exec_latency(args.target)
    with tempfile.TemporaryDirectory(prefix=run) as directory:
        root = Path(directory)
        large = root / (run + '.bin')
        large.write_bytes(os.urandom(LARGE_BYTES))
        digest = hashlib.sha256(large.read_bytes()).hexdigest()
        seconds = pb('send', str(large), '--to', args.target, timeout=900)
        if find_and_hash(args.target, large.name) != digest:
            raise RuntimeError('pb send delivered different bytes')
        metrics['send_mbit_s'] = round(LARGE_BYTES * 8 / seconds / 1e6, 1)
        destination = remote(args.target, (
            'import pathlib\n'
            f'd=pathlib.Path.home()/"Paperboat"/{run!r}\n'
            'd.mkdir(parents=True)\nprint(d)\n'))
        try:
            seconds = pb('scp', str(large), args.target + ':' + destination + '/scp.bin', timeout=900)
            metrics['scp_mbit_s'] = round(LARGE_BYTES * 8 / seconds / 1e6, 1)
            small = root / 'small'
            small.mkdir()
            for index in range(SMALL_FILES):
                (small / f'{index:04d}.bin').write_bytes(os.urandom(SMALL_BYTES))
            seconds = pb('rsync', '-a', str(small) + '/', args.target + ':' + destination + '/rsync/',
                         timeout=900)
            metrics['rsync_small_files_s'] = round(SMALL_FILES / seconds, 1)
            count = remote(args.target, (
                'import pathlib\n'
                f'print(len(list((pathlib.Path({destination!r})/"rsync").iterdir())))\n'))
            if count != str(SMALL_FILES):
                raise RuntimeError('managed rsync delivered a different file count')
        finally:
            remote(args.target, (
                'import pathlib,shutil\n'
                f'd=pathlib.Path({destination!r})\n'
                'd.parent.name=="Paperboat" and shutil.rmtree(d,ignore_errors=True)\n'))
    budget = BUDGETS.get(metrics['path'], {})
    failures = [] if args.no_budget else [
        name for name, (kind, bound) in (budget | BUDGETS['cli']).items()
        if name in metrics and (metrics[name] > bound if kind == 'max' else metrics[name] < bound)]
    if not budget and not args.no_budget:
        failures = [f'no budget for path {metrics["path"]}']
    print(json.dumps({'status': 'FAIL' if failures else 'PASS', 'target': args.target, 'check': 'perf',
                      'metrics': metrics, 'budget_failures': failures,
                      'enrollment_unchanged': registration() == before}))


if __name__ == '__main__':
    main()
