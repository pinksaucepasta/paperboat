#!/usr/bin/env python3
"""F3-05: native terminals have no plan limit and closed sessions never count.

Opens more terminals than the old 20-session limit, closes some, opens more in their place, and
fails on any limit refusal or 409 on the way (Lane W, 2026-10-23: only the machine's protection
ceiling of 256 remains, covered by TestRouterAttachesToEverySessionAtTheCeiling).
Usage: session_limit.py TARGET. Creates smoke-limit-* sessions, then removes them all.
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
OPEN = 25  # more than the removed 20-session limit
CLOSE = 5


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


def diagnostics_tail(count=12):
    events = []
    for path in sorted((Path.home() / '.local/state/paperboat/diagnostics').glob('*.ndjson'))[-2:]:
        for line in path.read_text(errors='replace').splitlines()[-4000:]:
            try:
                event = json.loads(line)
            except ValueError:
                continue
            if event.get('severity') in ('error', 'warning') and event.get('code') != 'unclassified_log':
                fields = event.get('fields', {})
                events.append('%s %s/%s op=%s cause=%s chain=%s fn=%s' % (event['at'][11:19], event['category'], event['code'],
                              fields.get('operation'), fields.get('cause'), str(fields.get('error_chain'))[:90], str(fields.get('source_function'))[-40:]))
    return events[-count:]


def names(target):
    out = subprocess.run([str(PB), 'session', 'list', target, '--all', '--json'], capture_output=True, text=True, timeout=60).stdout
    return {s['name']: s for s in json.loads(out)['sessions']}


def refused(text):
    return 'session limit' in text or 'ceiling' in text or '409' in text or 'resource state changed' in text.lower()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    prefix = 'smoke-limit-' + uuid.uuid4().hex[:6] + '-'
    created = []
    failure = None
    timings = []
    try:
        for index in range(OPEN):
            started = time.monotonic()
            code, text = new(prefix + str(index), args.target)
            timings.append(round(time.monotonic() - started, 1))
            if code:
                kind = 'refused' if refused(text) else 'failed'
                raise RuntimeError('terminal %d of %d %s: %s; diagnostics: %s' % (index + 1, OPEN, kind, text[-200:], ' | '.join(diagnostics_tail())))
            created.append(prefix + str(index))
        live = [n for n, s in names(args.target).items() if n.startswith(prefix) and s['state'] not in ('closed', 'closing')]
        if len(live) != OPEN:
            raise RuntimeError('%d of %d created terminals are listed as live' % (len(live), OPEN))
        # Closed sessions never count: close some and open the same number again at once.
        for name in created[:CLOSE]:
            cleanup(str(PB), args.target, name)
        del created[:CLOSE]
        for index in range(CLOSE):
            name = prefix + 'again-' + str(index)
            code, text = new(name, args.target)
            if code:
                raise RuntimeError('terminal after closing %d was %s: %s' % (CLOSE, 'refused' if refused(text) else 'not opened', text[-200:]))
            created.append(name)
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
    print(json.dumps({'check': 'session-limit', 'target': args.target, 'status': 'PASS', 'opened': OPEN, 'closed_not_counted': True,
                      'reopened_after_close': CLOSE, 'max_open_seconds': max(timings)}), flush=True)


if __name__ == '__main__':
    main()
