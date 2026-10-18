#!/usr/bin/env python3
"""F3-05: the 20-session limit gives a clear message, and closed sessions don't count.

Usage: session_limit.py TARGET. Creates smoke-limit-* sessions with `pb new`, then removes them all.
"""
import argparse
import json
from pathlib import Path
import subprocess
import re
import time
import uuid

from terminal_lifecycle import Terminal

from session_cleanup import cleanup

PB = Path.home() / '.local/bin/pb'
LIMIT = 20


def new(name, target):
    terminal = Terminal([str(PB), 'connect', target, '--name', name, '--status-bar', 'off'])
    try:
        terminal.expect(b'\x1b[?2004h', timeout=60)
        terminal.expect(terminal.marker('READY', 'stty -echo; '), timeout=30)
        return 0, ''
    except RuntimeError:
        try:
            terminal.wait_exit(10)
        except RuntimeError:
            pass
        return 1, re.sub(r'\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)', '', terminal.output.decode(errors='replace'))
    finally:
        terminal.close()


def names(target):
    out = subprocess.run([str(PB), 'session', 'list', target, '--json'], capture_output=True, text=True, timeout=60).stdout
    return {s['name']: s for s in json.loads(out)['sessions']}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    prefix = 'smoke-limit-' + uuid.uuid4().hex[:6] + '-'
    created = []
    message = ''
    failure = None
    try:
        existing = sum(1 for s in names(args.target).values() if s['state'] not in ('closed', 'closing'))
        for index in range(LIMIT + 2 - existing):
            name = prefix + str(index)
            code, text = new(name, args.target)
            if code:
                message = text
                break
            created.append(name)
        total = existing + len(created)
        if total != LIMIT:
            raise RuntimeError('limit hit at %d live sessions, expected %d; message: %s' % (total, LIMIT, message[-200:]))
        if not message or ('20' not in message and 'limit' not in message.lower()) or 'Traceback' in message:
            raise RuntimeError('the over-limit attempt gave no clear limit message: ' + message[-200:])
        # A closed session no longer counts: one more can be created after closing one.
        cleanup(str(PB), args.target, created.pop())
        code, text = new(prefix + 'again', args.target)
        if code:
            raise RuntimeError('closed session still counted against the limit: ' + text[-200:])
        created.append(prefix + 'again')
    except Exception as error:
        failure = error
    finally:
        for name in created:
            try:
                cleanup(str(PB), args.target, name)
            except Exception:
                pass
            time.sleep(0.5)
    if failure is not None:
        print(json.dumps({'check': 'session-limit', 'status': 'FAIL', 'error': str(failure)}), flush=True)
        raise SystemExit(1)
    print(json.dumps({'check': 'session-limit', 'target': args.target, 'status': 'PASS', 'limit': LIMIT,
                      'clear_message': True, 'closed_not_counted': True}), flush=True)


if __name__ == '__main__':
    main()
