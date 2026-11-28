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

import winhost

PB = winhost.pb_path()


def pb(*args):
    done = winhost.pb_text(PB, args, timeout=120, stdin=subprocess.DEVNULL)
    if done.returncode:
        raise RuntimeError('pb %s failed: %s' % (' '.join(args), done.stderr[:160]))
    return done.stdout + done.stderr  # the "Showing N of M" note may go to either stream


def data(*args):
    out = json.loads(winhost.pb_text(PB, [*args, '--json'], timeout=120, stdin=subprocess.DEVNULL).stdout)
    return out.get('data', out)


def while_total_holds(read, attempts=6):
    """(total, read()) for a read made while the account's machine total did not move: other tests of the account
    pair and revoke machines at any time, and a total read before a pairing is not the one the next command sees."""
    for _ in range(attempts):
        before = data('machine', 'list', '--limit', '1')['total']
        value = read()
        if data('machine', 'list', '--limit', '1')['total'] == before:
            return before, value
    raise RuntimeError('the account\'s machine total changed during each of %d reads' % attempts)


def main():
    machines = data('machine', 'list', '--limit', '1')
    if len(machines['machines']) != 1 or machines['total'] < 2:
        raise RuntimeError('machine list --limit 1 gave %d machines of total %d' % (len(machines['machines']), machines['total']))
    total, text = while_total_holds(lambda: pb('machine', 'list', '--limit', '1'))
    if 'Showing 1 of %d machines' % total not in text:
        raise RuntimeError('machine list does not say "Showing 1 of %d machines"' % total)
    total, everything = while_total_holds(lambda: data('machine', 'list', '--all'))
    if len(everything['machines']) != total:
        raise RuntimeError('machine list --all returned %d of %d machines' % (len(everything['machines']), total))
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
