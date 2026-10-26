#!/usr/bin/env python3
"""Per-path API request counts of the idle machines on staging over an interval (F21-18, F21-19, F21-22).

Run on the staging server host while the machines are idle (F21-19, F21-22) or hold one open native stream
(F21-18; `pb exec <machine> -- sleep <seconds>` from another machine). Reads the server unit's request log, counts
machine-side requests by path (ids collapsed), divides by the number of machines and scales to 10 minutes, and checks
them against perf_budgets.BUDGETS['request_mix']. Staging only; edge, node and trust calls and the edge's
Go-http-client/1.1 are not machine requests. Prints one JSON receipt with numbers only.
Usage: request_mix.py --machines 2 [--seconds 600] [--unit pb-staging-server] [--budget request_mix|stream_open]
"""
import argparse
import collections
import json
import os
import re
import subprocess
import sys
import time

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', 'device-smoke'))
from perf_budgets import BUDGETS  # noqa: E402

SKIP = re.compile(r'path=/v1/(edge|nodes|trust)/|client=Go-http-client/1\.1')
PATH = re.compile(r'path=(\S+)')
ID = re.compile(r'(machine|session|env|endpoint)_[0-9a-f-]+')


def counts(unit, start, end):
    log = subprocess.run(['sudo', 'journalctl', '-u', unit, '--since', start, '--until', end, '--no-pager', '-o', 'cat'],
                         capture_output=True, text=True, timeout=120, check=True).stdout
    out = collections.Counter()
    for line in log.splitlines():
        if 'msg=http_request' not in line or SKIP.search(line):
            continue
        m = PATH.search(line)
        if m:
            out[ID.sub(r'\1_ID', m.group(1))] += 1
    return out


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--machines', type=int, required=True)
    parser.add_argument('--seconds', type=int, default=600)
    parser.add_argument('--unit', default='pb-staging-server')
    parser.add_argument('--budget', default='request_mix', choices=['request_mix', 'stream_open'])
    args = parser.parse_args()
    fmt = '%Y-%m-%d %H:%M:%S'
    start = time.strftime(fmt, time.gmtime())
    time.sleep(args.seconds)
    end = time.strftime(fmt, time.gmtime())
    per_path = counts(args.unit, start, end)
    scale = 600 / args.seconds / args.machines
    metrics = {path: round(n * scale, 1) for path, n in sorted(per_path.items())}
    metrics['total'] = round(sum(per_path.values()) * scale, 1)
    failures = [f'{path}={metrics.get(path, 0)}>{bound}' for path, (kind, bound) in BUDGETS[args.budget].items()
                if metrics.get(path, 0) > bound]
    print(json.dumps({'status': 'FAIL' if failures else 'PASS', 'check': 'request-mix-' + args.budget,
                      'per_machine_per_10min': metrics, 'seconds': args.seconds, 'machines': args.machines,
                      'budget_failures': failures}))
    return 1 if failures else 0


if __name__ == '__main__':
    sys.exit(main())
