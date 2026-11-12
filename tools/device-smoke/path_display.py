#!/usr/bin/env python3
"""F21-06: pb status, pb doctor and the bug report show the selected path; a forced relay-only path is reported as such.

  path_display.py TARGET     (run as root-capable user on a paired Linux or macOS machine; needs sudo -n iptables or pfctl;
                             with PB_SMOKE_WINDOWS_PB the client is the Windows device and the block is a firewall rule)
  1. direct: after a ping, `pb status` names the direct path for TARGET, `pb doctor TARGET` peer_reachability says direct
  2. UDP blocked (iptables, always removed): pings go over the regional relay, `pb status` reports selected_path
     wss or relay with a relay_region, doctor says regional_relay, and the bug report's status.json carries both
"""
import contextlib
import io
import json
import os
import re
import subprocess
import sys
import time
import zipfile

from pairing_lifecycle import command_result
import winhost

PB = winhost.pb_path()
DARWIN = os.uname().sysname == 'Darwin'
# The stock macOS pf.conf evaluates com.apple/* anchors, so no change to the main ruleset is needed.
PF_ANCHOR = 'com.apple/pbsmoke-path'
PF_RULE = 'block drop out quick on ! lo0 proto udp from any to any port != 53\n'
BLOCK = ['sudo', '-n', 'iptables', '{}', 'OUTPUT', '-p', 'udp', '!', '--dport', '53', '-j', 'DROP']


def pb(*args):
    done = command_result(PB, *args, timeout=180)
    return done.returncode, done.stdout, done.stderr


@contextlib.contextmanager
def udp_blocked():
    device = winhost.device()
    if device:
        with device.udp_blocked():
            yield
        return
    if DARWIN:
        sudo = lambda *args, **kw: subprocess.run(['sudo', '-n', *args], capture_output=True, text=True, timeout=30, **kw)
        # A root failsafe drops the rule even if this process dies; the rule is UDP only, so the ssh/TCP session survives.
        failsafe = subprocess.Popen(['sudo', '-n', 'sh', '-c', 'sleep 600; /sbin/pfctl -a %s -F all' % PF_ANCHOR],
                                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
        try:
            if sudo('pfctl', '-a', PF_ANCHOR, '-f', '-', input=PF_RULE).returncode:
                raise RuntimeError('pfctl could not load the UDP drop rule into ' + PF_ANCHOR)
            token = re.search(r'Token : (\d+)', sudo('pfctl', '-E').stderr)
            sudo('pfctl', '-F', 'states')  # an established UDP flow keeps passing on its state until flushed
            try:
                yield
            finally:
                sudo('pfctl', '-a', PF_ANCHOR, '-F', 'all')
                if token:
                    sudo('pfctl', '-X', token.group(1))
        finally:
            failsafe.terminate()
        return
    rule = lambda flag: [flag if part == '{}' else part for part in BLOCK]
    subprocess.run(rule('-I'), check=True)
    try:
        yield
    finally:
        subprocess.run(rule('-D'), check=False)


def bundle_status(path):
    """status.json of the bug report bundle, read where pb wrote it."""
    with zipfile.ZipFile(io.BytesIO(winhost.device_path(path).read_bytes())) as zipped:
        return zipped.read('status.json').decode()


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
    with udp_blocked():
        wait_for('relay path in status', lambda: (ping(), machine_status(target)['selected_path'] in ('wss', 'relay'))[1])
        status = machine_status(target)
        if not status.get('relay_region'):
            raise RuntimeError('status has a relay path but no relay_region')
        if doctor_path(target) != 'regional_relay':
            raise RuntimeError('doctor says %s on a relay-only path' % doctor_path(target))
        bundle = json.loads(pb('bugreport', '--json')[1])
        report = bundle_status(bundle['bundle_path'])
        if status['selected_path'] not in report or status['relay_region'] not in report:
            raise RuntimeError('the bug report status.json lacks the selected path or relay region')
    print(json.dumps({'check': 'path-display', 'status': 'PASS', 'forced_path': status['selected_path'], 'region': status['relay_region']}))


if __name__ == '__main__':
    try:
        main()
    except Exception as err:
        print(json.dumps({'check': 'path-display', 'status': 'FAIL', 'detail': str(err)}))
        sys.exit(1)
