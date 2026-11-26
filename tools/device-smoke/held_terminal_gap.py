#!/usr/bin/env python3
"""Measure one-second synthetic markers across an update without printing output.

Start this on the controller, wait for OUTPUT/ready, then update TARGET.
Raw output and the millisecond timeline stay in the private OUTPUT directory.
"""
import argparse
import datetime
import json
import os
from pathlib import Path
import re
import time

from session_cleanup import cleanup
from terminal_lifecycle import Terminal


def utc():
    return datetime.datetime.now(datetime.timezone.utc).isoformat(timespec='milliseconds')


def collect_markers(terminal, pid, count, output):
    rows = []
    for index in range(count):
        terminal.expect('PB_GAP_%s_%s' % (index, pid), timeout=100)
        received, received_ns = utc(), time.monotonic_ns()
        suffix = terminal.expect('\n').strip()
        stamp = re.fullmatch(rb'_(\d+)\.(\d+)', suffix)
        if stamp is None:
            raise RuntimeError('target production timestamp unavailable')
        produced_ns = int(stamp[1]) * 1000000000 + int(stamp[2].ljust(9, b'0'))
        rows.append(dict(i=index, at=received, ns=received_ns,
                         produced_unix_ns=produced_ns,
                         produced_at=datetime.datetime.fromtimestamp(
                             produced_ns / 1e9, datetime.timezone.utc).isoformat(timespec='milliseconds')))
    (output / 'markers.json').write_text(json.dumps(rows))
    (output / 'terminal.raw').write_bytes(terminal.output)
    return rows


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('output', type=Path)
    parser.add_argument('--pb', default=str(Path.home() / '.local/bin/pb'))
    parser.add_argument('--count', type=int, default=100)
    args = parser.parse_args()
    if not 2 <= args.count <= 600:
        parser.error('count must be between 2 and 600')
    os.umask(0o077)
    args.output.mkdir(mode=0o700, parents=True, exist_ok=False)
    name = 'smoke-held-gap-' + str(os.getpid())
    terminal = Terminal([args.pb, 'connect', args.target, '--name', name, '--status-bar', 'off'])
    result = dict(started=utc())
    rows = []
    try:
        terminal.expect(b'\x1b[?2004h', timeout=90)
        terminal.expect(terminal.marker('READY', 'stty -echo; '))
        terminal.send("printf '\\nPB_GAP_PID_%s\\n' \"$$\"\r")
        terminal.expect('PB_GAP_PID_')
        pid = int(terminal.expect('\n').strip())
        (args.output / 'measurement-process.json').write_text(json.dumps(
            dict(pid=os.getpid(), client_pid=terminal.pid, started=utc())))
        # Signal once before timed reads; no storage operations belong in the loop.
        (args.output / 'ready').touch()
        terminal.send("i=0; while [ \"$i\" -lt " + str(args.count) + " ]; do "
                      "printf '\\nPB_GAP_%s_%s_%s\\n' \"$i\" \"$$\" \"$EPOCHREALTIME\"; "
                      "i=$((i+1)); sleep 1; done\r")
        rows = collect_markers(terminal, pid, args.count, args.output)
        terminal.settle(.5)
        terminal.send("printf '\\nPB_GAP_END_%s\\n' \"$$\"\r")
        terminal.expect('PB_GAP_END_')
        result['same_shell'] = int(terminal.expect('\n').strip()) == pid
        terminal.send('exit\r')
        result['exit'] = terminal.wait_exit(timeout=20)
        gap, before, after = max((b['ns'] - a['ns'], a, b) for a, b in zip(rows, rows[1:]))
        result.update(gap_ns=gap, gap_seconds=gap / 1e9, before=before, after=after, markers=len(rows))
    except Exception as error:
        result.update(failure_type=type(error).__name__, failure_at=utc())
        (args.output / 'measurement-exception.raw').write_text(str(error))
    finally:
        (args.output / 'markers.json').write_text(json.dumps(rows))
        (args.output / 'terminal.raw').write_bytes(terminal.output)
        terminal.close()
        try:
            cleanup(args.pb, args.target, name)
            result['session_cleanup'] = True
        except Exception as error:
            result['session_cleanup'] = False
            (args.output / 'session-cleanup-exception.raw').write_text(str(error))
        result.update(finished=utc(), markers_timeline=rows)
        (args.output / 'result.json').write_text(json.dumps(result))
    print(json.dumps({k: v for k, v in result.items() if k in (
        'gap_seconds', 'same_shell', 'exit', 'session_cleanup', 'failure_type')}))


if __name__ == '__main__':
    main()
