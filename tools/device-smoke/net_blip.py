#!/usr/bin/env python3
"""F21-48: after a 5 min network blip the client recovers like the control plane: `pb doctor` does not report
healthy while the network is down, `pb exec` to TARGET works again within 30 s of the network returning, and a
native terminal attaches within the same 30 s.

  net_blip.py TARGET [--down-seconds 300]

The blip is network_drop.cut(): iptables owner-drop (Linux), a pf anchor (macOS) or a firewall block of pb.exe's own
traffic (a Windows device, PB_SMOKE_WINDOWS_PB set), lifted on every exit path and by a failsafe after
down-seconds + 60. Prints one JSON line.
"""
import argparse
import json
import subprocess
import sys
import time
import uuid

import network_drop
from session_cleanup import cleanup
from terminal_lifecycle import Terminal
import winhost

PB = network_drop.PB
RECOVER_SECONDS = 30
LATE_SECONDS = 300  # only to tell slow from stuck in the report


def exec_ok(target, timeout=25):
    try:
        return b'blip-ok' in winhost.run_pb(PB, ['exec', target, '--', 'echo', 'blip-ok'], timeout).stdout
    except subprocess.TimeoutExpired:  # expected while the network is down
        return False


def doctor_overall():
    """The overall state `pb doctor --json` reports; doctor exits non-zero when it is not healthy."""
    out = winhost.run_pb(PB, ['doctor', '--json'], 120).stdout.decode(errors='replace')
    return json.loads(out[out.index('{'):]).get('overall', '')


def attach(target, name, deadline):
    """A native terminal to target answers a marker before the monotonic deadline (RuntimeError otherwise)."""
    start = time.monotonic()
    terminal = Terminal(winhost.terminal_argv(PB, ['connect', target, '--name', name, '--status-bar', 'off']),
                        env=winhost.terminal_env(PB))
    try:
        terminal.expect(terminal.marker('BLIP', 'stty -echo; '), timeout=max(1, deadline - start))
    finally:
        terminal.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--down-seconds', type=int, default=300)
    args = parser.parse_args()
    if not network_drop.DEVICE and network_drop.sudo(*(['pfctl', '-s', 'info'] if network_drop.DARWIN else ['iptables', '-S', 'OUTPUT'])).returncode:
        raise RuntimeError('needs sudo without a prompt for ' + ('pfctl' if network_drop.DARWIN else 'iptables'))
    if not exec_ok(args.target):
        raise SystemExit('precondition: exec to %s does not work before the blip' % args.target)
    name = 'smoke-blip-' + uuid.uuid4().hex[:10]
    report = {}
    block, failsafe = network_drop.cut(args.down_seconds + 60)
    blocked = True
    try:
        time.sleep(min(90, args.down_seconds // 2))
        report['doctor_while_down'] = doctor_overall()
        report['exec_while_down'] = exec_ok(args.target)
        time.sleep(max(0, args.down_seconds - min(90, args.down_seconds // 2)))
        network_drop.release(block)
        blocked = False
        restored = time.monotonic()
        report['exec_after_s'] = None
        while time.monotonic() - restored < RECOVER_SECONDS:
            if exec_ok(args.target, 10):  # short tries: one hung request must not use the whole 30 s
                report['exec_after_s'] = round(time.monotonic() - restored, 1)
                break
            time.sleep(1)
        if report['exec_after_s'] is None:  # the row fails; keep waiting so a slow recovery reads differently from a stuck one
            while time.monotonic() - restored < LATE_SECONDS and not exec_ok(args.target, 10):
                time.sleep(1)
            report['exec_late_s'] = round(time.monotonic() - restored, 1) if time.monotonic() - restored < LATE_SECONDS else None
        else:
            attach(args.target, name, restored + RECOVER_SECONDS)
            report['terminal_after_s'] = round(time.monotonic() - restored, 1)
    except RuntimeError as error:  # the terminal missed its deadline
        report['terminal_after_s'] = None
        report['error'] = str(error)
    finally:
        if blocked:
            network_drop.release(block)
        if failsafe is not None and failsafe.poll() is None:
            failsafe.terminate()
        try:
            cleanup(PB, args.target, name)
        except Exception:
            pass
    ok = (report.get('doctor_while_down') not in (None, '', 'healthy') and report.get('exec_while_down') is False
          and report.get('exec_after_s') is not None and report.get('terminal_after_s') is not None)
    print(json.dumps({'check': 'net-blip', 'target': args.target, 'down_seconds': args.down_seconds,
                      'status': 'PASS' if ok else 'FAIL', **report}), flush=True)
    return 0 if ok else 1


if __name__ == '__main__':
    sys.exit(main())
