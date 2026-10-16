#!/usr/bin/env python3
"""Verify managed SSH command and interactive shell behavior on an exact target."""
import argparse
import json
import os
from pathlib import Path
import re
import select
import shlex
import time
import uuid

from exec_lifecycle import run
from install_lifecycle import registration
from terminal_lifecycle import Terminal
from pairing_lifecycle import command_diagnostic


def shell_ready(terminal):
    deadline = time.monotonic() + 60
    last_output = time.monotonic()
    while True:
        visible = re.sub(rb'\x1b\][^\x07]*(?:\x07|\x1b\\)|\x1b\[[0-?]*[ -/]*[@-~]', b'', terminal.output)
        # A shell prompt is a settled, unterminated line; its glyphs are a user
        # preference. The following computed marker proves this is a real shell.
        if visible.rsplit(b'\n', 1)[-1].strip() and time.monotonic() - last_output >= .2:
            return
        if time.monotonic() >= deadline:
            raise RuntimeError('SSH did not present its interactive shell prompt')
        if select.select([terminal.fd], [], [], .1)[0]:
            chunk = os.read(terminal.fd, 65536)
            if not chunk:
                raise RuntimeError('SSH closed before its interactive shell prompt')
            terminal.output.extend(chunk)
            terminal.answer_queries(chunk)
            last_output = time.monotonic()
            if len(terminal.output) > 1 << 20:
                raise RuntimeError('SSH banner exceeded its output bound')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    binary = Path.home() / '.local/bin/pb'
    before = registration()
    marker = 'PB_SSH_' + uuid.uuid4().hex
    command = "printf '%s\\n' " + shlex.quote(marker + '_λ船') + "; printf '%s\\n' PB_SSH_STDERR >&2; exit 37"
    output, errors = run(binary, 'ssh', args.target, '--', command, expected=37)
    if output != (marker + '_λ船\n').encode() or errors != b'PB_SSH_STDERR\n':
        raise RuntimeError('SSH command changed stdout, Unicode, stderr or exit code')
    terminal = Terminal([binary, 'ssh', args.target])
    try:
        # Wait for the authenticated remote shell before sending its command.
        shell_ready(terminal)
        terminal.send("printf '\\nPB_SSH_READY_%s\\n' '" + marker + "'\r")
        terminal.expect('PB_SSH_READY_' + marker, timeout=60)
        terminal.send('exit 0\r')
        if terminal.wait_exit(20) != 0:
            raise RuntimeError('interactive SSH shell did not exit successfully')
    except Exception as error:
        diagnostic = command_diagnostic(terminal.arguments, bytes(terminal.output), b'')
        raise RuntimeError('interactive SSH failed; private diagnostics: ' + str(diagnostic)) from error
    finally:
        terminal.close()
    if registration() != before:
        raise RuntimeError('SSH changed the enrolled identity')
    print(json.dumps({'check': 'ssh-main', 'target': args.target, 'status': 'PASS',
                      'command': True, 'interactive': True, 'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
