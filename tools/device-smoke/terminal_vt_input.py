#!/usr/bin/env python3
"""Qualify that a Windows client's stdin delivers terminal keys byte for byte (F3-01).

The Windows client reads its console with ReadConsoleW (cli/internal/session/input_windows.go; ReadFile ends the stream at Ctrl-Z). The review
of that change asked for proof that arrows, Home/End, the editing and F keys, a bracketed multi-line paste, emoji and Ctrl-Z still arrive:
`pb connect` from the device into `cat -v` on the peer shows each sequence as the remote received it."""
import argparse
import json
import os
import select
import time
import uuid

from session_cleanup import cleanup
from terminal_lifecycle import Terminal
import winhost

KEYS = [
    ('Up', b'\x1b[A', b'^[[A'), ('Down', b'\x1b[B', b'^[[B'), ('Right', b'\x1b[C', b'^[[C'), ('Left', b'\x1b[D', b'^[[D'),
    ('Home', b'\x1b[H', b'^[[H'), ('End', b'\x1b[F', b'^[[F'), ('Insert', b'\x1b[2~', b'^[[2~'), ('Delete', b'\x1b[3~', b'^[[3~'),
    ('PageUp', b'\x1b[5~', b'^[[5~'), ('PageDown', b'\x1b[6~', b'^[[6~'), ('Ctrl-Up', b'\x1b[1;5A', b'^[[1;5A'),
    ('F1', b'\x1bOP', b'^[OP'), ('F2', b'\x1bOQ', b'^[OQ'), ('F3', b'\x1bOR', b'^[OR'), ('F4', b'\x1bOS', b'^[OS'),
    ('F5', b'\x1b[15~', b'^[[15~'), ('F6', b'\x1b[17~', b'^[[17~'), ('F7', b'\x1b[18~', b'^[[18~'), ('F8', b'\x1b[19~', b'^[[19~'),
    ('F9', b'\x1b[20~', b'^[[20~'), ('F10', b'\x1b[21~', b'^[[21~'), ('F11', b'\x1b[23~', b'^[[23~'), ('F12', b'\x1b[24~', b'^[[24~'),
    ('bracketed multi-line paste', b'\x1b[200~line1\nline2\nline3\x1b[201~', b'^[[200~line1\r\nline2\r\nline3^[[201~'),
    ('emoji', '😀'.encode(), b'M-pM-^_M-^XM-^@'),
]
EMOJI_CAT = b'M-pM-^_M-^XM-^@'


def drain(terminal, quiet=0.8, limit=30):
    """Read until the terminal has been quiet for `quiet` seconds (or `limit` seconds passed)."""
    received = bytearray()
    deadline, last = time.monotonic() + limit, time.monotonic()
    while time.monotonic() < deadline and time.monotonic() - last < quiet:
        if select.select([terminal.fd], [], [], 0.1)[0]:
            chunk = os.read(terminal.fd, 65536)
            if not chunk:
                break
            terminal.answer_queries(chunk)
            terminal.output.extend(chunk)
            received.extend(chunk)
            last = time.monotonic()
    return bytes(received)


def send_and_read(terminal, data):
    drain(terminal, 0.3)
    terminal.send(data)
    return drain(terminal)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    pb = winhost.pb_path()
    name = 'smoke-vt-' + uuid.uuid4().hex[:8]
    terminal = Terminal(winhost.terminal_argv(pb, ['connect', args.target, '--name', name, '--status-bar', 'off']), env=winhost.terminal_env(pb))
    failures = []
    try:
        terminal.expect(b'\x1b[?2004h', timeout=60)
        terminal.expect(terminal.marker('READY', 'stty -echo; '), timeout=60)
        terminal.send('stty raw -echo; cat -v\r')
        drain(terminal, 1.5)
        for label, data, expected in KEYS:
            got = send_and_read(terminal, data)
            if expected not in got:
                failures.append('%s: sent %r, the remote showed %r' % (label, data, got[-120:]))
        flood = ('a' * 975 + '😀' * 50 + 'b' * 975).encode()  # surrogate pairs split across console reads
        got = send_and_read(terminal, flood)
        if got.count(EMOJI_CAT) != 50 or b'M-pM-^_M-^XM-^@M-pM-^_M-^XM-^@' not in got:
            failures.append('50 emoji in a row: the remote showed %d whole emoji (want 50)' % got.count(EMOJI_CAT))
        got = send_and_read(terminal, b'\x1a')
        if b'^Z' not in got:  # ReadFile ended the stream here and the session closed
            failures.append('Ctrl-Z: the remote showed %r' % got[-120:])
        got = send_and_read(terminal, b'PROBE123')
        if b'PROBE123' not in got:
            failures.append('the session did not survive Ctrl-Z')
    finally:
        terminal.close()
        cleanup(pb, args.target, name)
    if failures:
        raise RuntimeError('keys lost on the Windows client: ' + '; '.join(failures))
    print(json.dumps({'check': 'terminal-vt', 'target': args.target, 'status': 'PASS', 'keys': len(KEYS), 'emoji': 50, 'ctrl_z': True}))


if __name__ == '__main__':
    main()
