#!/usr/bin/env python3
"""F21-168 on a real stack: one account holds N concurrent tunnels (default 1,000), created by one machine's CLI.

  tunnel_scale.py [--count 1000] [--parallel 8] [--batch 100] [--pause 15] [--hold 120] [--port 18099]
Creates N durable tunnels to one local origin port, --batch at a time with --pause seconds between batches (production-safe), then (a) lists them with the CLI (time, rows returned), (b) holds for --hold seconds
while the account sits at N tunnels (nothing may poll per row), and (c) deletes
every tunnel it created. Prints one JSON receipt: create/list/delete timings and error counts, numbers only.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
import json
import statistics
import time
import uuid

from install_lifecycle import registration
from tunnel_lifecycle import confirmed, run


def timed(function, *args):
    started = time.monotonic()
    try:
        return function(*args), time.monotonic() - started, None
    except Exception as error:  # the receipt counts failures by message; none is retained beyond 80 bytes
        return None, time.monotonic() - started, str(error)[:80]


def quantiles(values):
    values = sorted(values)
    return {'p50': round(values[len(values) // 2], 2), 'p95': round(values[int(len(values) * 0.95)], 2), 'max': round(values[-1], 2)} if values else None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--count', type=int, default=1000)
    parser.add_argument('--parallel', type=int, default=8)
    parser.add_argument('--batch', type=int, default=100)
    parser.add_argument('--pause', type=int, default=15)
    parser.add_argument('--hold', type=int, default=120)
    parser.add_argument('--port', type=int, default=18099)
    args = parser.parse_args()
    before = registration()
    prefix = 'smoke-scale-' + uuid.uuid4().hex[:6]
    created, errors = {}, {}
    started = time.monotonic()
    for first in range(0, args.count, args.batch):
        indexes = range(first, min(first + args.batch, args.count))
        with ThreadPoolExecutor(args.parallel) as pool:
            for i, (value, took, error) in zip(indexes, pool.map(
                    lambda i: timed(run, 'tunnel', 'create', f'{prefix}-{i:04d}', '--port', str(args.port), '--duration', '180m'), indexes)):
                if error:
                    errors[error] = errors.get(error, 0) + 1
                else:
                    created[f'{prefix}-{i:04d}'] = (value['tunnel']['id'], took)
        time.sleep(args.pause)
    receipt = {'check': 'tunnel-scale', 'count': args.count, 'created': len(created), 'create_errors': errors,
               'create_seconds_total': round(time.monotonic() - started, 1),
               'create_each': quantiles([took for _, took in created.values()])}
    try:
        listed, took, error = timed(run, 'tunnel', 'list', '--limit', '200')
        rows = [] if listed is None else listed.get('items', [])
        receipt['list'] = {'seconds': round(took, 2), 'error': error, 'rows_returned': len(rows),
                           'total': None if listed is None else listed.get('total')}
        if listed is not None and (listed.get('total') or 0) < len(created):
            receipt['list']['error'] = 'total %s is below the %d tunnels created' % (listed.get('total'), len(created))
        time.sleep(args.hold)
    finally:
        started = time.monotonic()
        failures = {}
        items = list(created.values())
        for first in range(0, len(items), args.batch):
            with ThreadPoolExecutor(args.parallel) as pool:
                for _, _, error in pool.map(lambda item: timed(confirmed, 'tunnel', 'delete', item[0]), items[first:first + args.batch]):
                    if error:
                        failures[error] = failures.get(error, 0) + 1
            time.sleep(args.pause)
        receipt['delete'] = {'failures': failures, 'seconds_total': round(time.monotonic() - started, 1)}
    receipt['enrollment_unchanged'] = registration() == before
    receipt['status'] = 'PASS' if (len(created) == args.count and not receipt['list']['error'] and not failures
                                   and receipt['enrollment_unchanged']) else 'FAIL'
    print(json.dumps(receipt))
    raise SystemExit(0 if receipt['status'] == 'PASS' else 1)


if __name__ == '__main__':
    main()
