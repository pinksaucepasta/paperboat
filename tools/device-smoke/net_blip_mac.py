#!/usr/bin/env python3
"""F21-48 on macOS: after a 5 min network blip, exec and a native terminal to TARGET work again within 30 s of the
network returning, and `pb doctor` does not report healthy while the network is down.

  net_blip_mac.py TARGET [--down-seconds 300] [--keep CIDR]   (sudo password from the file named by SUDO_PW_FILE)

The blip is a pf anchor that drops all outbound traffic except to --keep (the VM host link the controller's ssh uses);
it is flushed on every exit path and by a detached root failsafe after down-seconds + 60. Prints one JSON line.
"""
import argparse
import json
import os
import subprocess
import sys
import time

from clock_skew_mac import PB, pb, sudo, terminal

ANCHOR = 'com.apple/pbblip'


def block(keep, failsafe):
    rules = 'block drop out quick inet from any to ! %s\\nblock drop out quick inet6 all\\n' % keep
    sudo("printf '%s' | pfctl -a %s -f - 2>/dev/null; pfctl -E >/dev/null 2>&1; pfctl -F states >/dev/null 2>&1; "
         "(sleep %d; pfctl -a %s -F all) >/dev/null 2>&1 </dev/null &" % (rules, ANCHOR, failsafe, ANCHOR))


def unblock():
    sudo('pfctl -a %s -F all >/dev/null 2>&1' % ANCHOR)


def within(seconds, probe):
    start = time.time()
    while time.time() - start < seconds:
        if probe():
            return round(time.time() - start, 1)
        time.sleep(2)
    return None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--down-seconds', type=int, default=300)
    parser.add_argument('--keep', default='192.168.64.0/24')
    args = parser.parse_args()
    def exec_ok():
        try:
            return 'blip-ok' in pb('exec', args.target, '--', 'echo', 'blip-ok', timeout=25).stdout
        except subprocess.TimeoutExpired:  # expected while the network is down
            return False
    if not exec_ok():
        raise SystemExit('precondition: exec to %s does not work before the blip' % args.target)
    report = {}
    block(args.keep, args.down_seconds + 60)
    try:
        time.sleep(min(90, args.down_seconds // 2))
        doctor = pb('doctor', timeout=90).stdout
        report['doctor_while_down'] = next((l for l in doctor.splitlines() if l.startswith('Paperboat doctor')), '')[:80]
        report['exec_while_down'] = exec_ok()
        time.sleep(max(0, args.down_seconds - min(90, args.down_seconds // 2)))
    finally:
        unblock()
    report['exec_after_s'] = within(30, exec_ok)
    report['terminal_after'] = terminal(args.target)
    ok = (report['doctor_while_down'] != '' and 'healthy' not in report['doctor_while_down'].replace('unhealthy', '')
          and report['exec_after_s'] is not None and report['terminal_after'])
    print(json.dumps({'check': 'net-blip-mac', 'target': args.target, 'down_seconds': args.down_seconds,
                      'status': 'PASS' if ok else 'FAIL', **report}))
    return 0 if ok else 1


if __name__ == '__main__':
    sys.exit(main())
