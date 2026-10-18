#!/usr/bin/env python3
"""F3-03: a held terminal survives a network drop: same shell, no lost or duplicated input.

Blocks all outbound non-loopback traffic of the CLI user for a bounded time with iptables
(rule removed on every exit path and by a root failsafe timer), typing before, during and after.
Usage: network_drop.py TARGET [--drop-seconds N]   (needs passwordless or cached sudo)
"""
import argparse
import json
import os
from pathlib import Path
import datetime
import glob
import subprocess
import time
import uuid

from session_cleanup import cleanup
from terminal_lifecycle import Terminal

PB = Path.home() / '.local/bin/pb'
RULE = ['OUTPUT', '-m', 'owner', '--uid-owner', str(os.getuid()), '!', '-o', 'lo', '-j', 'DROP']


def sudo(*args):
    return subprocess.run(['sudo', '-n', *args], capture_output=True, text=True, timeout=30)


def failures_since(moment):
    count = 0
    for path in glob.glob(str(Path.home() / '.local/state/paperboat/diagnostics/*.ndjson')):
        for line in open(path, errors='replace'):
            try:
                event = json.loads(line)
            except ValueError:
                continue
            if event.get('at', '') >= moment and event.get('severity') in ('error', 'warning') \
                    and event.get('category') in ('control_request', 'peer_connect', 'stream_open', 'transport'):
                count += 1
    return count


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--drop-seconds', type=int, default=20)
    args = parser.parse_args()
    if sudo('iptables', '-S', 'OUTPUT').returncode:
        raise RuntimeError('needs sudo without a prompt for iptables')
    name = 'smoke-drop-' + uuid.uuid4().hex[:10]
    log = Path.home() / ('.' + name + '.log')
    terminal = None
    blocked = False
    failsafe = None
    try:
        terminal = Terminal([PB, 'connect', args.target, '--name', name, '--status-bar', 'off'])
        terminal.expect(b'\x1b[?2004h')
        terminal.expect(terminal.marker('READY', 'stty -echo; '))
        terminal.send("pb_hold=$$; printf '\\nPB_PID_%s\\n' \"$$\"\r")
        terminal.expect('PB_PID_')
        held = terminal.expect('\n').decode().strip()

        def type_line(number):
            terminal.send("echo IN_%d >> %s; printf '\\nPB_ACK_%d\\n'\r" % (number, log, number))

        for number in (1, 2, 3):
            type_line(number)
            terminal.expect('PB_ACK_%d' % number)
        # Root failsafe removes the rule even if this process dies.
        failsafe = subprocess.Popen(['sudo', '-n', 'sh', '-c', 'sleep %d; iptables -D %s' % (args.drop_seconds + 60, ' '.join(RULE))],
                                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
        started = datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%S')
        sudo('iptables', '-I', *RULE)
        blocked = True
        time.sleep(4)
        for number in (4, 5, 6):  # typed while the network is down
            type_line(number)
            time.sleep(1)
        time.sleep(max(0, args.drop_seconds - 7))
        sudo('iptables', '-D', *RULE)
        blocked = False
        for number in (4, 5, 6):
            terminal.expect('PB_ACK_%d' % number, timeout=150)
        type_line(7)
        terminal.expect('PB_ACK_7', timeout=60)
        terminal.send("printf '\\nPB_AGAIN_%s\\n' \"$$\"\r")
        terminal.expect('PB_AGAIN_', timeout=30)
        if terminal.expect('\n').decode().strip() != held:
            raise RuntimeError('terminal came back in a different shell')
        if not failures_since(started):
            raise RuntimeError('the network block never produced a connection failure; the drop was not exercised')
        lines = log.read_text().split()
        if lines != ['IN_%d' % n for n in range(1, 8)]:
            raise RuntimeError('input lost, duplicated or reordered: %d lines' % len(lines))
    finally:
        if blocked:
            sudo('iptables', '-D', *RULE)
        if failsafe is not None and failsafe.poll() is None:
            failsafe.terminate()
        if terminal is not None:
            terminal.close()
        log.unlink(missing_ok=True)
        try:
            cleanup(str(PB), args.target, name)
        except Exception:
            pass
    print(json.dumps({'check': 'network-drop', 'target': args.target, 'status': 'PASS',
                      'drop_seconds': args.drop_seconds, 'same_shell': True, 'input_exactly_once': True}), flush=True)


if __name__ == '__main__':
    main()
