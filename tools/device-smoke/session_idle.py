#!/usr/bin/env python3
"""F3-04: a detached session idle for 8 hours is still usable (same shell, variable intact).

`start` creates the session and records it; `verify` (run >= 8 h later) re-attaches and checks it.
Usage: session_idle.py start|verify TARGET [--state FILE] [--hours 8]
"""
import argparse
import json
from pathlib import Path
import time
import uuid

from session_cleanup import cleanup
from session_lifecycle import selected
from terminal_lifecycle import Terminal

PB = Path.home() / '.local/bin/pb'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('phase', choices=['start', 'verify'])
    parser.add_argument('target')
    parser.add_argument('--state', type=Path, default=Path.home() / '.pbsmoke-idle.json')
    parser.add_argument('--hours', type=float, default=8)
    args = parser.parse_args()
    if args.phase == 'start':
        name = 'smoke-idle-' + uuid.uuid4().hex[:10]
        nonce = uuid.uuid4().hex
        terminal = Terminal([PB, 'connect', args.target, '--name', name, '--status-bar', 'off'])
        try:
            terminal.expect(b'\x1b[?2004h')
            terminal.expect(terminal.marker('READY', 'stty -echo; '))
            terminal.send("PB_IDLE='%s'; pb_shell=$$; printf '\\nPB_SET_%%s_%%s\\n' \"$PB_IDLE\" \"$$\"\r" % nonce)
            terminal.expect('PB_SET_' + nonce + '_')
            shell = terminal.expect('\n').decode().strip()
        finally:
            terminal.close()
        _, session = selected(PB, args.target, name)
        args.state.write_text(json.dumps({'name': name, 'nonce': nonce, 'shell': shell, 'id': session['id'],
                                          'started': time.time()}))
        args.state.chmod(0o600)
        print(json.dumps({'check': 'session-idle', 'phase': 'start', 'status': 'STARTED', 'state': str(args.state)}), flush=True)
        return
    record = json.loads(args.state.read_text())
    idle = (time.time() - record['started']) / 3600
    if idle < args.hours:
        raise RuntimeError('only %.2f hours idle; need %.1f' % (idle, args.hours))
    terminal = Terminal([PB, 'session', 'attach', args.target, record['id'], '--status-bar', 'off'])
    try:
        terminal.expect(b'\x1b[?2004h', timeout=90)
        terminal.send("printf '\\nPB_BACK_%s_%s\\n' \"$PB_IDLE\" \"$$\"\r")
        terminal.expect('PB_BACK_' + record['nonce'] + '_', timeout=90)
        if terminal.expect('\n').decode().strip() != record['shell']:
            raise RuntimeError('idle session came back in a different shell')
    finally:
        terminal.close()
        cleanup(str(PB), args.target, record['name'])
        args.state.unlink(missing_ok=True)
    print(json.dumps({'check': 'session-idle', 'phase': 'verify', 'status': 'PASS', 'idle_hours': round(idle, 2),
                      'same_shell_and_variable': True}), flush=True)


if __name__ == '__main__':
    main()
