#!/usr/bin/env python3
"""PQ-04 (CLI): list commands read one bounded page from the server and report the server's total.

Run on a paired machine whose account has more than one machine and more than one inbox request.
  1. `pb machine list --limit 1` shows one machine and "Showing 1 of N machines" with N from the server;
     `--all` returns all N
  2. `pb inbox requests --limit 1` shows one request and "Showing 1 of N"
  3. `pb preview list` and `pb tunnel list` with `--limit 1` return at most one item; a next_cursor, when present,
     leads to a different item
"""
import json
from pathlib import Path
import subprocess
import sys

PB = str(Path.home() / '.local/bin/pb')


def pb(*args):
    done = subprocess.run([PB, *args], capture_output=True, text=True, timeout=120, stdin=subprocess.DEVNULL)
    if done.returncode:
        raise RuntimeError('pb %s failed: %s' % (' '.join(args), done.stderr[:160]))
    return done.stdout + done.stderr  # the "Showing N of M" note may go to either stream


def data(*args):
    out = json.loads(subprocess.run([PB, *args, '--json'], capture_output=True, text=True, timeout=120, stdin=subprocess.DEVNULL).stdout)
    return out.get('data', out)


def main():
    machines = data('machine', 'list', '--limit', '1')
    total = machines['total']
    if len(machines['machines']) != 1 or total < 2:
        raise RuntimeError('machine list --limit 1 gave %d machines of total %d' % (len(machines['machines']), total))
    if 'Showing 1 of %d machines' % total not in pb('machine', 'list', '--limit', '1'):
        raise RuntimeError('machine list does not say "Showing 1 of %d machines"' % total)
    if len(data('machine', 'list', '--all')['machines']) != total:
        raise RuntimeError('machine list --all did not return all %d machines' % total)
    requests = data('inbox', 'requests', '--limit', '1')
    if len(requests['requests']) != 1 or requests['total'] < 2 or 'Showing 1 of %d' % requests['total'] not in pb('inbox', 'requests', '--limit', '1'):
        raise RuntimeError('inbox requests --limit 1 is not one request of the server total')
    for noun in ('preview', 'tunnel'):
        page = data(noun, 'list', '--limit', '1')
        if len(page['items']) > 1:
            raise RuntimeError('%s list --limit 1 returned %d items' % (noun, len(page['items'])))
        if page.get('next_cursor'):
            second = data(noun, 'list', '--limit', '1', '--cursor', page['next_cursor'])
            if second['items'] == page['items']:
                raise RuntimeError('%s list --cursor returned the same page' % noun)
    print(json.dumps({'check': 'cli-paging', 'status': 'PASS', 'machines_total': total, 'inbox_total': requests['total']}))


if __name__ == '__main__':
    try:
        main()
    except Exception as err:
        print(json.dumps({'check': 'cli-paging', 'status': 'FAIL', 'detail': str(err)}))
        sys.exit(1)
