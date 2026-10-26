#!/usr/bin/env python3
"""F3-20: terminal operations on a revoked machine end (run on a signed-in owner machine).

TARGET is a throwaway machine of the same account, given by name and exact machine ID; this check
revokes it, so it refuses any name without a throwaway prefix (smoke-, lanek-, lanea-) or whose name
and ID do not match `pb machine list`. Real devices (hp, apple, victus) can never be passed. It opens a terminal on TARGET, detaches, revokes TARGET by ID, then expects:
- `pb session close` and `pb session delete` on that terminal to finish (exit 0, or a typed refusal
  naming the revoked machine) within 30 s each, never hang or ask to retry;
- the terminal to stop being listed as open or closing within 60 s;
- no terminal operation for it to stay pending (seen as a terminal stuck in `closing`).
Usage: terminal_dead_machine.py TARGET MACHINE_ID
"""
import json
from pathlib import Path
import re
import subprocess
import sys
import time
import uuid

sys.path.insert(0, Path(__file__).resolve().parent.as_posix())
from session_lifecycle import selected  # noqa: E402
from terminal_matrix import connect  # noqa: E402

PB = Path.home() / '.local/bin/pb'
FAILS = []


def row(name, ok, detail=''):
    print(('PASS ' if ok else 'FAIL ') + name + (f': {detail}' if detail else ''), flush=True)
    if not ok:
        FAILS.append(name)


def pb(*args, timeout=60):
    done = subprocess.run([str(PB), *args], capture_output=True, text=True, timeout=timeout)
    return done.returncode, done.stdout, done.stderr


THROWAWAY = re.compile(r'^(smoke-|lanek-|lanea-)[a-z0-9-]+$')


def check_throwaway(target, machine_id):
    """Refuse anything but a throwaway machine whose name and ID match the account's list."""
    if not THROWAWAY.match(target):
        raise SystemExit(f'refusing: {target!r} is not a throwaway machine name (smoke-, lanek-, lanea-)')
    code, out, _ = pb('machine', 'list', '--all', '--json')
    if code != 0:
        raise SystemExit('refusing: pb machine list failed')
    data = json.loads(out)
    data = data.get('data', data)
    items = data.get('items') or data.get('machines') or [] if isinstance(data, dict) else data
    match = [m for m in items if m.get('id') == machine_id]
    if len(match) != 1 or (match[0].get('alias') or match[0].get('name')) != target:
        raise SystemExit(f'refusing: {machine_id} is not the machine named {target!r}')


def revoke(machine_id):
    code, out, err = pb('machine', 'revoke', machine_id)
    token = re.search(r'--confirm ([A-Za-z0-9]{6})', out + err)
    if not token:
        raise RuntimeError('revoke gave no confirmation code')
    code, out, err = pb('machine', 'revoke', machine_id, '--confirm', token.group(1))
    if code != 0:
        raise RuntimeError('revoke failed: ' + (out + err).strip()[-200:])


def session_state(session_id):
    """The terminal's state in the owner's list of all terminals, or None once it is gone."""
    code, out, _ = pb('session', 'list', '--q', session_id, '--json')
    if code != 0:
        return 'list-failed'
    data = json.loads(out)
    data = data.get('data', data)
    for item in data.get('sessions', []) if isinstance(data, dict) else []:
        if item.get('id') == session_id:
            return item.get('state')
    return None


def parse(out):
    try:
        value = json.loads(out)
    except ValueError:
        return {}
    return value if isinstance(value, dict) else {}


def main():
    target, machine_id = sys.argv[1], sys.argv[2]
    check_throwaway(target, machine_id)
    name = 'smoke-deadops-' + uuid.uuid4().hex[:10]
    client = connect(target, name)
    _, session = selected(PB, target, name)
    client.close()
    revoke(machine_id)
    for action in ('close', 'delete'):
        t0 = time.monotonic()
        try:
            code, out, err = pb('session', action, target, session['id'], '--json', timeout=30)
            receipt = parse(out)
            token = (receipt.get('data') or {}).get('confirmation_token')
            if token:
                code, out, err = pb('session', action, target, session['id'], '--confirm', token, '--json', timeout=30)
                receipt = parse(out)
            failure = receipt.get('error') or {}
            finished = receipt.get('ok') is True or (failure.get('retryable') is False and re.search(
                r'revoked|machine_unavailable|not_found', str(failure.get('code', '')) + ' ' + str(failure.get('message', '')), re.I))
            row(f'F3-20 session {action} on a revoked machine finishes', bool(finished),
                f'exit {code} in {time.monotonic() - t0:.1f} s, ok={receipt.get("ok")}, code={failure.get("code")}')
        except subprocess.TimeoutExpired:
            row(f'F3-20 session {action} on a revoked machine finishes', False, 'hung for 30 s')
    deadline = time.monotonic() + 60
    state = session_state(session['id'])
    while state in ('open', 'running', 'closing', 'detached') and time.monotonic() < deadline:
        time.sleep(5)
        state = session_state(session['id'])
    row('F3-20 the terminal is not left open or closing', state not in ('open', 'running', 'closing', 'detached', 'list-failed'),
        f'state {state!r} after {60 - max(0, deadline - time.monotonic()):.0f} s')
    print('RESULT', 'PASS' if not FAILS else 'FAIL ' + ','.join(FAILS))
    return 1 if FAILS else 0


if __name__ == '__main__':
    sys.exit(main())
