#!/usr/bin/env python3
"""Closing a terminal session hangs its shell up (9608b7351): bash and zsh leave at once and leave no orphan, on every Unix target.

  xplat_term_close.py [--devices L,M] [--shells bash,zsh]

For each ordered pair (SRC opens, DST hosts) and each shell present on DST: `pb connect DST --name smoke-close-<id>`, the session's shell is
replaced by that interactive shell (`exec <shell> -i`), its pid and a background `sleep` (a job the shell must take with it) are read
on DST, then `pb session close` (confirmed) closes the session. Checks, measured on DST: the shell is gone when the close command returns
(allowing 2 s), the background job is gone within 3 s (the shell resent SIGHUP to it), the close command took at most 10 s, and no process
of that session remains. Results: results/xplat-term-close-<stamp>.json, ids `term-close:<shell>:<SRC>><DST>`.
"""
import json
import re
import sys
import time
import uuid
from itertools import permutations
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import xplat_fleet as fleet  # noqa: E402
from xplat_term import Terminal  # noqa: E402

results = {}


def alive(dst, pid):
    return dst.run(['kill', '-0', str(pid)]).rc == 0


def session_id(src, dst, name):
    listed = json.loads(src.pb('session', 'list', dst.alias, '--json').text)
    owned = [s for s in listed['sessions'] if s.get('name') == name]
    assert len(owned) == 1, 'session %s not listed exactly once (%d)' % (name, len(owned))
    return owned[0]['id']


def close_session(src, dst, session):
    first = json.loads(src.pb('session', 'close', dst.alias, session, '--json').text)
    token = first['data']['confirmation_token']
    done = src.pb('session', 'close', dst.alias, session, '--json', '--confirm', token, timeout=60)
    assert done.rc == 0, 'close failed: ' + (done.text + done.errtext)[-200:]


def t_close(src, dst, shell):
    if dst.run(['sh', '-c', 'command -v %s' % shell]).rc != 0:
        return 'n/a: %s is not installed on %s' % (shell, dst.name)
    name = 'smoke-close-' + uuid.uuid4().hex[:8]
    term = Terminal(src, dst, extra=['--name', name])
    try:
        term.ready()
        term.say('exec %s -i' % shell)
        time.sleep(2)
        marker = uuid.uuid4().hex[:6]
        sleep_seconds = 600 + int(marker[:3], 16) % 300  # a unique command line for the job
        term.say('sleep %d & echo P_%s_$$_E' % (sleep_seconds, marker))
        term.expect(r'P_%s_(\d+)_E' % marker, timeout=60)
        pid = int(term.child.match.group(1))
        assert alive(dst, pid), 'the shell %d is not running on %s' % (pid, dst.name)
        job = lambda: dst.run(['sh', '-c', "pgrep -f '^sleep %d$' | head -1" % sleep_seconds]).text.strip()
        assert job(), 'the background job did not start'
        session = session_id(src, dst, name)
        began = time.time()
        close_session(src, dst, session)
        took = time.time() - began
        gone_at_return = not alive(dst, pid)
        deadline = time.time() + 2
        while alive(dst, pid) and time.time() < deadline:
            time.sleep(0.25)
        assert not alive(dst, pid), '%s (pid %d) still runs 2 s after the close returned' % (shell, pid)
        deadline = time.time() + 3
        while job() and time.time() < deadline:
            time.sleep(0.25)
        assert not job(), 'the shell left its background job (sleep %d) running: an orphan' % sleep_seconds
        assert took <= 10, 'closing the session took %.1f s' % took
        return '%s pid %d: shell %s when the close returned, background job gone, close took %.1f s' % (
            shell, pid, 'gone' if gone_at_return else 'gone within 2 s', took)
    finally:
        term.close()


def check(name, pair, action):
    key = 'term-close:%s:%s' % (name, pair)
    started = time.time()
    try:
        detail = action()
        results[key] = {'status': 'PASS', 'detail': detail or ''}
        print('PASS %s %ds %s' % (key, time.time() - started, detail or ''), flush=True)
    except Exception as error:  # noqa: BLE001
        results[key] = {'status': 'FAIL', 'detail': str(error)[:500]}
        print('FAIL %s %ds: %s' % (key, time.time() - started, str(error)[:500]), flush=True)


def main():
    argv = sys.argv[1:]
    keys = argv[argv.index('--devices') + 1].split(',') if '--devices' in argv else ['L', 'M']
    shells = argv[argv.index('--shells') + 1].split(',') if '--shells' in argv else ['bash', 'zsh']
    devices = {k: d for k, d in fleet.fleet().items() if k in keys}
    for a, b in permutations(devices, 2):
        for shell in shells:
            check(shell, '%s>%s' % (a, b), lambda: t_close(devices[a], devices[b], shell))
    stamp = time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())
    out = Path(__file__).parent / 'results' / ('xplat-term-close-%s.json' % stamp)
    out.write_text(json.dumps({'check': 'xplat-term-close', 'devices': keys, 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
