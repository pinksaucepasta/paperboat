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
        terminal.send("Write-Output ('PB_PROM'+'PT_%s')\r" % token)
        terminal.expect('PB_PROMPT_' + token, timeout=60)
        # the remote command prints a marker the typed text does not contain, then sleeps; the exit code is printed after pb returns
        remote = "printf 'PB_START%%s\\n' ED_%s; sleep 60" % token
        command = "& '%s' exec %s -- sh -c \"%s\"; Write-Output ('PB_RC'+'_'+$LASTEXITCODE)" % (pb, args.target, remote)
        terminal.send(command + '\r')
        terminal.expect('PB_STARTED_' + token, timeout=90)
        started = time.monotonic()
        terminal.send(b'\x03')  # the Ctrl-C key as a console receives it through ConPTY
        deadline = started + args.seconds
        while time.monotonic() < deadline and code is None:
            try:
                terminal.expect(b'PB_RC_', timeout=1)
            except RuntimeError:
                continue
            match = re.search(rb'PB_RC_(-?\d+)', bytes(terminal.output))
            code = int(match.group(1)) if match else None
        took = time.monotonic() - started
        if code is None:
            raise RuntimeError('Ctrl-C in an interactive console did not stop pb.exe within %d s' % args.seconds)
        if code == 0:
            raise RuntimeError('pb.exe exited 0 after Ctrl-C (it finished or ignored the interrupt)')
    finally:
        terminal.close()
    print(json.dumps({'check': 'ctrlc-interactive', 'target': args.target, 'status': 'PASS', 'exit_code': code, 'seconds': round(took, 1),
                      'sent': 'ETX through ConPTY (a real console Ctrl-C)'}))


if __name__ == '__main__':
    main()
