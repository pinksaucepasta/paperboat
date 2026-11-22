#!/usr/bin/env python3
"""The console start across the container boundary (fixture_checks.py recording-key-rotation).

recordings_key_rotation.py runs inside the team owner's container (its files, its pb), but a recording starts only from the
console, and node, Chromium and the owner's console session are on the controller. This is the whole conversation, through
files in ~/bridge (BRIDGE_DIR) of the container:
  console_start_bridge.py --team T --session S   container side (the --console-start-cmd): leaves a request, waits for the answer,
                                                 exits with the controller's exit code and prints its one-line message
  console_start_bridge.py claim                  controller side (run in the container): one JSON line per new request, then removed
  console_start_bridge.py answer ID CODE         controller side: the answer; the message is read from stdin
"""
import json
import os
from pathlib import Path
import re
import sys
import time
import uuid

NAME = re.compile(r'^[A-Za-z0-9_.-]+$')
DIR = Path(os.environ.get('BRIDGE_DIR') or Path.home() / 'bridge')


def put(name, value):
    DIR.mkdir(mode=0o700, exist_ok=True)
    tmp = DIR / (name + '.tmp')
    tmp.write_text(json.dumps(value))
    tmp.rename(DIR / name)  # a reader never sees half a file


def ask(team, session, seconds=240):
    if not (NAME.match(team) and NAME.match(session)):
        return 2, 'team and session may only hold letters, digits, _ . -'
    rid = uuid.uuid4().hex
    put(rid + '.req', {'team': team, 'session': session})
    answer, deadline = DIR / (rid + '.res'), time.monotonic() + seconds
    while time.monotonic() < deadline:
        if answer.exists():
            reply = json.loads(answer.read_text())
            answer.unlink()
            return reply['code'], reply['message']
        time.sleep(0.5)
    (DIR / (rid + '.req')).unlink(missing_ok=True)
    return 1, 'the controller did not answer the console start within %d s' % seconds


def claim():
    found = []
    for path in sorted(DIR.glob('*.req')) if DIR.is_dir() else []:
        request = json.loads(path.read_text())
        path.unlink()
        found.append(dict(request, id=path.stem))
    return found


def answer(rid, code, message):
    if not re.match(r'^[0-9a-f]{32}$', rid):
        raise ValueError('bad request id')
    put(rid + '.res', {'code': code, 'message': message[-300:]})


def main(argv):
    if argv[:1] == ['claim']:
        for request in claim():
            print(json.dumps(request))
        return 0
    if argv[:1] == ['answer'] and len(argv) == 3:
        answer(argv[1], int(argv[2]), sys.stdin.read())
        return 0
    if len(argv) == 4 and argv[0] == '--team' and argv[2] == '--session':
        code, message = ask(argv[1], argv[3])
        print(message, file=sys.stderr)
        return code
    print(__doc__, file=sys.stderr)
    return 2


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
