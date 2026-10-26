#!/usr/bin/env python3
"""F10-01: set, list and unset personal values and per-machine overrides; they reach new terminals and execs; never logged.

  env_scopes.py TARGET [--password-file ABSOLUTE_OWNER_ONLY_FILE]

Run on a machine with an unlocked ENV vault (--password-file unlocks it first with `pb env vault unlock`).
TARGET is another machine of the same account; the machine this runs on is the second machine, so every
step also checks that a scope does not leak to the other machine.
  1. a personal value is set: `pb env list` names it (names only, no value anywhere in the output); a new exec
     on TARGET and on this machine sees it, and so does a new terminal (`pb connect`) on TARGET
  2. a per-machine override for TARGET is set: TARGET sees the override, this machine still sees the personal
     value, `pb env host show --machine TARGET` has a layer that belongs to TARGET
  3. the override is unset (exact confirmation): TARGET sees the personal value again
  4. the personal value is unset: TARGET sees nothing and `pb env list` no longer names it
  5. neither value appears in the output of any command above or in the diagnostics of either machine
Team scopes have their own check (env_team_scopes.py, F10-04). Values are random per run and never printed.
"""
import argparse
import json
import os
from pathlib import Path
import pty
import re
import select
import subprocess
import time
import uuid

PB = Path.home() / '.local/bin/pb'
WAIT_SECONDS = 180
DIAGNOSTICS = '$HOME/.local/state/paperboat/diagnostics'


class Output:
    """Everything the commands printed, so one search proves no value was echoed."""
    seen = []


def run(*args, stdin=None, timeout=90, records_value=False):
    """Run pb. A command that prints a value on purpose (exec printenv) is not searched for leaks."""
    result = subprocess.run([str(PB), *args], input=stdin, capture_output=True, text=True, timeout=timeout)
    if not records_value:
        Output.seen.extend([result.stdout, result.stderr])
    return result.returncode, result.stdout, result.stderr


def own_machine_id():
    root = Path(os.environ.get('PAPERBOAT_RUNTIME_ROOT', Path.home() / '.local/state/paperboat/runtime'))
    return json.loads((root / 'machine-registration.json').read_text())['machine_id']


def resolve(target):
    code, out, err = run('machine', 'list', '--json')
    if code:
        raise RuntimeError('machine list failed: ' + err[:120])
    for machine in json.loads(out)['machines']:
        if target in (machine['id'], machine.get('alias')):
            return machine['id']
    raise RuntimeError('target %s is not a machine of this account' % target)


def sees(machine, name):
    """The value a new exec on MACHINE gets for NAME (None when it is unset); the first exec also applies a delivery."""
    code, out, _ = run('exec', machine, '--', 'printenv', name, records_value=True)
    return out.rstrip('\n') if code == 0 else None


def wait_sees(machine, name, expected, what):
    deadline = time.monotonic() + WAIT_SECONDS
    while True:
        value = sees(machine, name)
        if value == expected:
            return
        if time.monotonic() > deadline:
            raise RuntimeError('%s: %s still %s after %d s' % (
                what, machine, 'unset' if value is None else 'a different value', WAIT_SECONDS))
        time.sleep(3)


def terminal_sees(machine, name, marker):
    """The value a brand new terminal session (pb connect) on MACHINE has, read from the terminal."""
    pid, fd = pty.fork()
    if pid == 0:
        os.execv(str(PB), [str(PB), 'connect', machine, 'new', '--name', 'smoke-env-' + uuid.uuid4().hex[:6]])
    command = 'printf "%s<%%s>\\n" "$%s"; exit\n' % (marker, name)
    sent, collected, deadline = False, b'', time.monotonic() + 90
    while time.monotonic() < deadline:
        ready, _, _ = select.select([fd], [], [], 1)
        if ready:
            try:
                chunk = os.read(fd, 4096)
            except OSError:
                break
            if not chunk:
                break
            collected += chunk
        if not sent and (b'$' in collected or b'#' in collected or time.monotonic() > deadline - 70):
            os.write(fd, command.encode())
            sent = True
    try:
        os.close(fd)
    except OSError:
        pass
    os.waitpid(pid, 0)
    values = re.findall(re.escape(marker) + r'<([^>]*)>', collected.decode(errors='replace'))
    values = [value for value in values if value != '%s']  # the typed command line echoes the format, not a value
    return values[-1] if values else None


def unset(name, *scope):
    _, out, err = run('env', 'unset', name, *scope, '--json')  # exits non-zero: confirmation_required
    token = (json.loads(out or '{}').get('data') or {}).get('confirmation_token')
    if not token:
        raise RuntimeError('unset %s gave no confirmation to confirm: %s' % (' '.join(scope) or 'personal', err[:120]))
    code, _, err = run('env', 'unset', name, *scope, '--json', '--confirm', token)
    if code:
        raise RuntimeError('confirmed unset failed: ' + err[:120])


def listed(name):
    code, out, err = run('env', 'list', '--json')
    if code:
        raise RuntimeError('env list failed: ' + err[:120])
    return name in json.loads(out)['data']['names']


def leaked(values, target):
    """Where the values appear: in any printed output or in either machine's diagnostics (named, never printed)."""
    found = []
    text = '\n'.join(Output.seen)
    patterns = [flag for value in values for flag in ('-e', value)]
    local = subprocess.run(['grep', '-rlF', *patterns, os.path.expandvars(DIAGNOSTICS)], capture_output=True, text=True).stdout
    remote_command = 'grep -rlF %s %s 2>/dev/null; true' % (' '.join(patterns), DIAGNOSTICS)
    remote = run('exec', target, '--', 'sh', '-c', remote_command, records_value=True)[1]
    for label, value in zip(('personal', 'override'), values):
        if value in text:
            found.append(label + ' value in command output')
    if local.strip():
        found.append('diagnostics on this machine')
    if remote.strip():
        found.append('diagnostics on the target')
    return found


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--password-file', help='absolute owner-only file with the ENV vault password; unlocks the vault first')
    args = parser.parse_args()
    if args.password_file:
        code, _, err = run('env', 'vault', 'unlock', '--password-file', args.password_file, '--json')
        if code:
            raise RuntimeError('unlocking the ENV vault failed: ' + err[:120])
    name = 'PB_SMOKE_' + uuid.uuid4().hex[:8].upper()
    personal, override = 'p-' + uuid.uuid4().hex, 'o-' + uuid.uuid4().hex
    marker = 'PBENV' + uuid.uuid4().hex[:6]
    me, target_id = own_machine_id(), resolve(args.target)
    result = {'check': 'env-scopes', 'status': 'PASS'}
    set_personal = set_override = False
    try:
        code, _, err = run('env', 'set', name, '--value-stdin', '--json', stdin=personal)
        if code:
            raise RuntimeError('could not set the personal value: ' + err[:120])
        set_personal = True
        if not listed(name):
            raise RuntimeError('pb env list does not name the value just set')
        wait_sees(args.target, name, personal, 'a new exec does not see the personal value')
        if sees(me, name) != personal:
            raise RuntimeError('a new exec on this machine does not see the personal value')
        if terminal_sees(args.target, name, marker) != personal:
            raise RuntimeError('a new terminal on the target does not see the personal value')
        result['personal_in_exec_and_terminal'] = True

        code, _, err = run('env', 'set', name, '--machine', target_id, '--value-stdin', '--json', stdin=override)
        if code:
            raise RuntimeError('could not set the per-machine override: ' + err[:120])
        set_override = True
        wait_sees(args.target, name, override, 'the per-machine override is not delivered')
        if sees(me, name) != personal:
            raise RuntimeError('the per-machine override leaked to the other machine')
        code, out, _ = run('env', 'host', 'show', '--machine', args.target, '--json')
        if code or not any(layer['source']['machine_id'] == target_id for layer in json.loads(out)['layers']):
            raise RuntimeError('host show has no layer that belongs to the target')
        if terminal_sees(args.target, name, marker) != override:
            raise RuntimeError('a new terminal on the target does not see the override')
        result['override_isolated_to_machine'] = True

        unset(name, '--machine', target_id)
        set_override = False
        wait_sees(args.target, name, personal, 'unsetting the override did not restore the personal value')
        unset(name)
        set_personal = False
        wait_sees(args.target, name, None, 'unsetting the personal value left it in new execs')
        if listed(name):
            raise RuntimeError('pb env list still names the unset value')
        result['unset_restores_and_removes'] = True
        problems = leaked((personal, override), args.target)
        if problems:
            raise RuntimeError('a value leaked: ' + ', '.join(problems))
        result['never_in_output_or_diagnostics'] = True
    finally:
        for flag, scope in ((set_override, ('--machine', target_id)), (set_personal, ())):
            if flag:
                try:
                    unset(name, *scope)
                except Exception:  # cleanup must not hide the primary error
                    pass
    print(json.dumps(result), flush=True)


if __name__ == '__main__':
    main()
