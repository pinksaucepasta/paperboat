#!/usr/bin/env python3
"""Check durable terminal identity, replay and normal session management."""
import argparse
import json
from pathlib import Path
import sys
import tempfile
import uuid

from install_lifecycle import registration
from machine_lifecycle import restore_all
from pairing_lifecycle import await_external_action, command_diagnostic, invoke
from session_cleanup import cleanup, inventory, mutate
from terminal_lifecycle import Terminal


def selected(pb, target, name, environment=None):
    actual, sessions = inventory(str(pb), target, environment)
    matches = [session for session in sessions if session['name'] == name]
    if len(matches) != 1 or matches[0]['is_default'] is not False:
        raise RuntimeError('catalog did not identify the exact named non-default session')
    return actual, matches[0]


def qualify(pb, target, check):
    name = 'smoke-session-' + uuid.uuid4().hex[:12]
    renamed = name + '-renamed'
    clients = []
    terminal = None
    try:
        terminal = Terminal([pb, 'connect', target, '--name', name, '--status-bar', 'off'])
        clients.append(terminal)
        terminal.expect(b'\x1b[?2004h')
        terminal.expect(terminal.marker('READY', 'stty -echo; '))
        nonce = uuid.uuid4().hex
        terminal.send("PB_DURABLE='" + nonce + "'; printf '\\nPB_DURABLE_%s\\n' \"$PB_DURABLE\"\r")
        terminal.expect('PB_DURABLE_' + nonce)
        environment, session = selected(pb, target, name)
        identifier = session['id']
        if check == 'manage':
            result = invoke(pb, 'session', 'rename', target, identifier, renamed, '--json')
            if result['session']['id'] != identifier or result['session']['name'] != renamed:
                raise RuntimeError('rename receipt changed the session identity')
            _, current = selected(pb, target, renamed, environment)
            if current['id'] != identifier:
                raise RuntimeError('rename catalog changed the session identity')
        terminal.close()
        terminal = Terminal([pb, 'session', 'attach', target, identifier, '--status-bar', 'off'])
        clients.append(terminal)
        terminal.expect(b'\x1b[?2004h')
        if check in ('replay', 'cross'):
            terminal.expect('PB_DURABLE_' + nonce, timeout=15)
        terminal.send("printf '\\nPB_REATTACHED_%s\\n' \"$PB_DURABLE\"\r")
        terminal.expect('PB_REATTACHED_' + nonce)
        if check == 'cross':
            terminal.close()
            with tempfile.TemporaryDirectory(prefix='paperboat-terminal-cross-') as directory:
                await_external_action(Path(directory), 'terminal_attach_required',
                                      {'target': target, 'session_id': identifier,
                                       'expected_state': nonce,
                                       'expected_account': registration()['account_id']})
        if check == 'manage':
            mutate(str(pb), 'close', target, environment, identifier)
            terminal.wait_exit()
            # The shared cleanup verifies closed state, deletes the exact ID,
            # and confirms absence; it never selects another user's terminal.
        result = {'check': 'session-' + check, 'target': target,
                  'status': 'PASS', 'same_shell': True,
                  'replayed_output': check in ('replay', 'cross'),
                  'other_device': check == 'cross'}
    except Exception as error:
        if terminal is not None:
            diagnostic = command_diagnostic(terminal.arguments,
                                            terminal.output.decode(errors='replace'), '')
            raise RuntimeError('session ' + check + ' failed; private diagnostics: ' + str(diagnostic)) from error
        raise
    finally:
        restore_all(sys.exc_info()[1],
                    *[client.close for client in clients],
                    lambda: cleanup(str(pb), target, renamed),
                    lambda: cleanup(str(pb), target, name))
    print(json.dumps(result), flush=True)


def resume(pb, target, identifier, nonce, account):
    enrolled = registration()
    if enrolled['account_id'] != account:
        raise RuntimeError('controller account differs from the fixture owner; nothing attached')
    _, sessions = inventory(str(pb), target)
    session = next((item for item in sessions if item['id'] == identifier), None)
    if session is None or not session['name'].startswith('smoke-session-') or session['is_default'] is not False:
        raise RuntimeError('cross-device fixture does not identify an exact owned non-default terminal')
    terminal = Terminal([pb, 'session', 'attach', target, identifier, '--status-bar', 'off'])
    try:
        terminal.expect(b'\x1b[?2004h')
        terminal.expect('PB_DURABLE_' + nonce, timeout=15)
        terminal.send("printf '\\nPB_OTHER_DEVICE_%s\\n' \"$PB_DURABLE\"\r")
        terminal.expect('PB_OTHER_DEVICE_' + nonce)
        if registration() != enrolled:
            raise RuntimeError('cross-device attach changed the controller enrollment')
    except Exception as error:
        diagnostic = command_diagnostic(terminal.arguments, terminal.output.decode(errors='replace'), '')
        raise RuntimeError('other device attach failed; private diagnostics: ' + str(diagnostic)) from error
    finally:
        restore_all(sys.exc_info()[1], terminal.close)
    print(json.dumps({'check': 'session-resume', 'target': target, 'status': 'PASS',
                      'same_shell': True, 'replayed_output': True}), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--check', choices=['manage', 'replay', 'cross', 'resume'], required=True)
    parser.add_argument('--session-id')
    parser.add_argument('--expected-state')
    parser.add_argument('--expected-account')
    args = parser.parse_args()
    pb = Path.home() / '.local/bin/pb'
    if args.check == 'resume':
        if not all([args.session_id, args.expected_state, args.expected_account]):
            parser.error('resume requires the exact fixture ID, synthetic state and account')
        resume(pb, args.target, args.session_id, args.expected_state, args.expected_account)
    else:
        qualify(pb, args.target, args.check)


if __name__ == '__main__':
    main()
