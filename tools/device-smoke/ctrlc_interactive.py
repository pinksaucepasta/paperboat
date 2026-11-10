#!/usr/bin/env python3
"""A real console Ctrl-C key stops pb.exe in an interactive Windows console (F4-03, F3-11, F1-04, C045 evidence).

The harness interrupts pb.exe with CTRL_BREAK (winhost.INTERRUPT: CTRL_C_EVENT is not delivered to a process started through the non-interactive
sshd chain, which passes the Ctrl-C-ignore flag on). A user at a console does not have that flag, and this check proves it: an interactive
PowerShell over `ssh -tt` (a ConPTY, the user's path), `pb exec PEER -- sh -c '...; sleep 60'` typed at its prompt, and the byte 0x03
sent through the pty, as a Ctrl-C key press arrives. pb must cancel: the prompt comes back long before the 60 s sleep ends and the command's exit code is not 0."""
import argparse
import json
import re
import time
import uuid

from terminal_lifecycle import Terminal
import winhost


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--seconds', type=int, default=30, help='how long after the key pb may take to cancel')
    args = parser.parse_args()
    pb = winhost.pb_path()
    device = pb.device
    token = uuid.uuid4().hex[:8]
    terminal = Terminal(device.ssh_prefix('-tt', device.target, 'powershell -NoProfile -NoLogo'), env=winhost.terminal_env(pb))
    code = took = None
    try:
        terminal.expect(b'PS ', timeout=60)  # the prompt: a line typed earlier reaches a still-cooked pty and PSReadLine takes its LF as a continuation
        terminal.send("Write-Output ('PB_PROM'+'PT_%s')\r" % token)
        terminal.expect('PB_PROMPT_' + token, timeout=60)
        # the remote command prints a marker the typed text does not contain, then sleeps
        remote = "printf 'PB_START%%s\\n' ED_%s; sleep 60" % token
        terminal.send("& '%s' exec %s -- sh -c \"%s\"\r" % (pb, args.target, remote))
        terminal.expect('PB_STARTED_' + token, timeout=90)
        started = time.monotonic()
        terminal.send(b'\x03')  # the Ctrl-C key as a console receives it through ConPTY
        try:
            terminal.expect(b'canceled', timeout=args.seconds)  # pb: Remote execution was canceled.
            terminal.expect(b'PS ', timeout=args.seconds)  # and the prompt is back (PSReadLine drops the rest of a typed line on Ctrl-C, so the code is read by a new line)
        except RuntimeError as error:
            raise RuntimeError('Ctrl-C in an interactive console did not stop pb.exe within %d s' % args.seconds) from error
        took = time.monotonic() - started
        terminal.send("Write-Output ('PB_RC'+'_'+$LASTEXITCODE)\r")
        terminal.expect(b'PB_RC_', timeout=30)
        terminal.settle(0.7)  # the digits follow the marker
        found = re.findall(rb'PB_RC_(-?\d+)', bytes(terminal.output))
        code = int(found[-1]) if found else None
        if code in (None, 0):
            raise RuntimeError('pb.exe exit code after Ctrl-C was %r (want non-zero: it finished or ignored the interrupt)' % (code,))
    finally:
        terminal.close()
    print(json.dumps({'check': 'ctrlc-interactive', 'target': args.target, 'status': 'PASS', 'exit_code': code, 'seconds': round(took, 1),
                      'sent': 'ETX through ConPTY (a real console Ctrl-C)'}))


if __name__ == '__main__':
    main()
