#!/usr/bin/env python3
"""F3-03: a held terminal survives a network drop: same shell, no lost or duplicated input.

Blocks all outbound non-loopback traffic of the CLI user for a bounded time with iptables (macOS: a pf
anchor under com.apple), rule removed on every exit path and by a root failsafe timer, typing before,
during and after.
Usage: network_drop.py TARGET [--drop-seconds N]   (needs passwordless or cached sudo)

With PB_SMOKE_WINDOWS_PB set the client is the Windows device (winhost.py; TARGET is a Unix machine): the drop is a
firewall block of pb.exe's own traffic (winhost.cut_network), so the SSH that drives the device keeps working; a
timer on the device removes the rule whatever happens here.
"""
import argparse
import json
import os
from pathlib import Path
import datetime
import glob
import re
import subprocess
import time
import uuid

from session_cleanup import cleanup
from terminal_lifecycle import Terminal
import winhost

PB = winhost.pb_path()
DEVICE = winhost.device()
RULE = ['OUTPUT', '-m', 'owner', '--uid-owner', str(os.getuid()), '!', '-o', 'lo', '-j', 'DROP']
DARWIN = not DEVICE and os.uname().sysname == 'Darwin'
DIAGNOSTICS = Path.home() / ('Library/Application Support/Paperboat/state/diagnostics' if DARWIN
                             else '.local/state/paperboat/diagnostics')
# The stock macOS pf.conf evaluates com.apple/* anchors, so no change to the main ruleset is needed.
ANCHOR = 'com.apple/pbsmoke'
PF_RULE = 'block drop out quick on ! lo0 proto { tcp udp } all user %d\n' % os.getuid()


def sudo(*args, input=None):
    return subprocess.run(['sudo', '-n', *args], capture_output=True, text=True, timeout=30, input=input)


def pf_block(rules, failsafe_seconds):
    """macOS: load rules into the anchor, enable pf, cut existing states. Returns (enable token, root failsafe)."""
    failsafe = subprocess.Popen(['sudo', '-n', 'sh', '-c', 'sleep %d; /sbin/pfctl -a %s -F all' % (failsafe_seconds, ANCHOR)],
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
    if sudo('pfctl', '-a', ANCHOR, '-f', '-', input=rules).returncode:
        failsafe.terminate()
        raise RuntimeError('pfctl could not load the drop rule into ' + ANCHOR)
    token = re.search(r'Token : (\d+)', sudo('pfctl', '-E').stderr)
    sudo('pfctl', '-F', 'states')  # an established connection keeps passing on its state until flushed
    return token.group(1) if token else None, failsafe


def pf_release(block):
    token, failsafe = block
    sudo('pfctl', '-a', ANCHOR, '-F', 'all')
    if token:
        sudo('pfctl', '-X', token)
    if failsafe.poll() is None:
        failsafe.terminate()


def shell_pid(raw):
    """The shell's pid in the text after PB_PID_: a ConPTY client repaints escape sequences around it."""
    found = re.search(rb'\d+', raw)
    if not found:
        raise RuntimeError('no shell pid in %r' % raw)
    return found.group()


def diagnostic_lines():
    """Every line of the daemon's diagnostic event files (on the device for a Windows client)."""
    if DEVICE:
        return DEVICE.matching_lines(DEVICE.runtime_dir().parent / 'diagnostics', '*.ndjson', '"severity":"(error|warning)"')
    return (line for path in glob.glob(str(DIAGNOSTICS / '*.ndjson')) for line in open(path, errors='replace'))


def failures_since(moment):
    count = 0
    for line in diagnostic_lines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if event.get('at', '') >= moment and event.get('severity') in ('error', 'warning') \
                and event.get('category') in ('control_request', 'peer_connect', 'stream_open', 'transport'):
            count += 1
    return count


def release(block):
    if DEVICE:
        block.release()
    elif DARWIN:
        pf_release(block)
    else:
        sudo('iptables', '-D', *RULE)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--drop-seconds', type=int, default=20)
    args = parser.parse_args()
    if not DEVICE and sudo(*(['pfctl', '-s', 'info'] if DARWIN else ['iptables', '-S', 'OUTPUT'])).returncode:
        raise RuntimeError('needs sudo without a prompt for ' + ('pfctl' if DARWIN else 'iptables'))
    name = 'smoke-drop-' + uuid.uuid4().hex[:10]
    log = '~/.' + name + '.log'  # expanded by the target's shell: the terminal and pb exec share its home, not /tmp
    terminal = None
    blocked = False
    failsafe = block = None
    try:
        terminal = Terminal(winhost.terminal_argv(PB, ['connect', args.target, '--name', name, '--status-bar', 'off']), env=winhost.terminal_env(PB))
        terminal.expect(b'\x1b[?2004h')
        terminal.expect(terminal.marker('READY', 'stty -echo; '))
        terminal.send("pb_hold=$$; printf '\\nPB_PID_%s\\n' \"$$\"\r")
        terminal.expect('PB_PID_')
        held = shell_pid(terminal.expect('\n'))

        def type_line(number):
            terminal.send("echo IN_%d >> %s; printf '\\nPB_ACK_%d\\n'\r" % (number, log, number))

        for number in (1, 2, 3):
            type_line(number)
            terminal.expect('PB_ACK_%d' % number)
        # Root failsafe removes the rule even if this process dies.
        if not DARWIN and not DEVICE:
            failsafe = subprocess.Popen(['sudo', '-n', 'sh', '-c', 'sleep %d; iptables -D %s' % (args.drop_seconds + 60, ' '.join(RULE))],
                                        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
        started = datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%S')
        if DEVICE:
            block = DEVICE.cut_network(args.drop_seconds + 60)
        elif DARWIN:
            block = pf_block(PF_RULE, args.drop_seconds + 60)
        else:
            sudo('iptables', '-I', *RULE)
        blocked = True
        time.sleep(4)
        for number in (4, 5, 6):  # typed while the network is down
            type_line(number)
            time.sleep(1)
        time.sleep(max(0, args.drop_seconds - 7))
        release(block)
        blocked = False
        for number in (4, 5, 6):
            terminal.expect('PB_ACK_%d' % number, timeout=150)
        type_line(7)
        terminal.expect('PB_ACK_7', timeout=60)
        terminal.send("printf '\\nPB_AGAIN_%s\\n' \"$$\"\r")
        terminal.expect('PB_AGAIN_', timeout=30)
        again = shell_pid(terminal.expect('\n'))
        if again != held:
            raise RuntimeError('terminal came back in a different shell (%r, was %r)' % (again, held))
        if not failures_since(started):
            raise RuntimeError('the network block never produced a connection failure; the drop was not exercised')
        # the shell that wrote the log runs on the target: read it there
        lines = winhost.run_pb(PB, ['exec', args.target, '--', 'sh', '-c', 'cat ' + log], 60).stdout.decode().split()
        if lines != ['IN_%d' % n for n in range(1, 8)]:
            raise RuntimeError('input lost, duplicated or reordered: %d lines' % len(lines))
    finally:
        if blocked:
            release(block)
        if failsafe is not None and failsafe.poll() is None:
            failsafe.terminate()
        if terminal is not None:
            terminal.close()
        winhost.run_pb(PB, ['exec', args.target, '--', 'sh', '-c', 'rm -f ' + log], 60)
        try:
            cleanup(PB, args.target, name)
        except Exception:
            pass
    print(json.dumps({'check': 'network-drop', 'target': args.target, 'status': 'PASS',
                      'drop_seconds': args.drop_seconds, 'same_shell': True, 'input_exactly_once': True}), flush=True)


if __name__ == '__main__':
    main()
