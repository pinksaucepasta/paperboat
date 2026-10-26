#!/usr/bin/env python3
"""F21-06: pb status, pb doctor and the bug report show the selected path; a forced relay-only path is reported as such.

  path_display.py TARGET     (run as root-capable user on a paired Linux machine; needs sudo -n iptables)
  1. direct: after a ping, `pb status` names the direct path for TARGET, `pb doctor TARGET` peer_reachability says direct
  2. UDP blocked (iptables, always removed): pings go over the regional relay, `pb status` reports selected_path
     wss or relay with a relay_region, doctor says regional_relay, and the bug report's status.json carries both
"""
import json
from pathlib import Path
import subprocess
import sys
import time
import zipfile

PB = str(Path.home() / '.local/bin/pb')
BLOCK = ['sudo', '-n', 'iptables', '{}', 'OUTPUT', '-p', 'udp', '!', '--dport', '53', '-j', 'DROP']


def pb(*args):
    done = subprocess.run([PB, *args], capture_output=True, text=True, timeout=180, stdin=subprocess.DEVNULL)
    return done.returncode, done.stdout, done.stderr


def machine_status(target):
    data = json.loads(pb('status', '--json')[1])
    data = data.get('data', data)
    return next(m for m in data['machines'] if target in (m['alias'], m['id']))


def doctor_path(target):
    checks = json.loads(pb('doctor', target, '--json')[1])['checks']
    return next(c.get('selected_path') for c in checks if c['code'] == 'peer_reachability')


def wait_for(what, condition, seconds=90):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if condition():
            return
        time.sleep(2)
    raise RuntimeError('%s: not seen in %d s' % (what, seconds))


def main():
    target = sys.argv[1]
    ping = lambda: pb('ping', target)
    wait_for('direct', lambda: (ping(), machine_status(target)['selected_path'] == 'direct')[1])
    if doctor_path(target) != 'direct':
        raise RuntimeError('doctor does not say direct on a direct path')
    rule = lambda flag: [flag if part == '{}' else part for part in BLOCK]
    subprocess.run(rule('-I'), check=True)
    try:
        wait_for('relay path in status', lambda: (ping(), machine_status(target)['selected_path'] in ('wss', 'relay'))[1])
        status = machine_status(target)
        if not status.get('relay_region'):
            raise RuntimeError('status has a relay path but no relay_region')
        if doctor_path(target) != 'regional_relay':
            raise RuntimeError('doctor says %s on a relay-only path' % doctor_path(target))
        bundle = json.loads(pb('bugreport', '--json')[1])
        with zipfile.ZipFile(bundle['bundle_path']) as zipped:
            report = zipped.read('status.json').decode()
        if status['selected_path'] not in report or status['relay_region'] not in report:
            raise RuntimeError('the bug report status.json lacks the selected path or relay region')
    finally:
        subprocess.run(rule('-D'), check=False)
    print(json.dumps({'check': 'path-display', 'status': 'PASS', 'forced_path': status['selected_path'], 'region': status['relay_region']}))


if __name__ == '__main__':
    try:
        main()
    except Exception as err:
        print(json.dumps({'check': 'path-display', 'status': 'FAIL', 'detail': str(err)}))
        sys.exit(1)
