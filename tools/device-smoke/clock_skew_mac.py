#!/usr/bin/env python3
"""F21-02 on macOS: with this Mac's clock 10 min ahead and then 10 min behind, pb exec, a terminal, pb ssh and pb send
to TARGET (a machine with a correct clock) all work, and `pb doctor` says how far off the clock is.

  clock_skew_mac.py TARGET    (sudo password from the file named by SUDO_PW_FILE)

Network time is turned off while the clock is moved and is always turned back on and synced at the end; a detached
failsafe re-syncs after 300 s even if this script dies. Prints one JSON line; exit 1 on any failure.
"""
import json
import os
import pty
import select
import subprocess
import sys
import tempfile
import time

PB = os.path.expanduser('~/.local/bin/pb')
OFFSETS = (600, -600)


def sudo(command):
    with open(os.environ['SUDO_PW_FILE']) as password:
        return subprocess.run(['sudo', '-S', '-p', '', 'sh', '-c', command], stdin=password,
                              capture_output=True, text=True, timeout=60)


def resync():
    sudo('systemsetup -setusingnetworktime on >/dev/null 2>&1; sntp -sS time.apple.com >/dev/null 2>&1')


def pb(*args, timeout=120):
    return subprocess.run([PB, *args], capture_output=True, text=True, timeout=timeout, stdin=subprocess.DEVNULL)


def terminal(target):
    child, descriptor = pty.fork()
    if child == 0:
        os.execv(PB, [PB, 'connect', target, 'new', '--name', 'skew-%d' % os.getpid()])
    output, end = bytearray(), time.time() + 90
    sent = False
    while time.time() < end and b'SKEW_2' not in output:
        if select.select([descriptor], [], [], .3)[0]:
            try:
                output.extend(os.read(descriptor, 65536))
            except OSError:
                break
        if not sent and b'$' in output:
            os.write(descriptor, b'echo SKEW_$((1+1))\r')
            sent = True
    try:
        os.write(descriptor, b'exit\r')
    except OSError:
        pass
    time.sleep(1)
    return b'SKEW_2' in output


def steps(target, offset, payload):
    results = {}
    results['exec'] = 'skew-ok' in pb('exec', target, '--', 'echo', 'skew-ok').stdout
    results['ssh'] = 'skew-ok' in pb('ssh', target, '--', 'echo skew-ok').stdout
    results['send'] = pb('send', payload, '--to', target).returncode == 0
    results['terminal'] = terminal(target)
    doctor = pb('doctor').stdout
    results['doctor'] = "clock is off by" in doctor
    results['doctor_line'] = next((l.strip() for l in doctor.splitlines() if 'clock' in l.lower()), '')[:160]
    return results


def main():
    target = sys.argv[1]
    sudo('(sleep 300; systemsetup -setusingnetworktime on; sntp -sS time.apple.com) >/dev/null 2>&1 </dev/null &')
    payload = os.path.join(tempfile.mkdtemp(), 'skew-%d.bin' % os.getpid())
    with open(payload, 'wb') as handle:
        handle.write(os.urandom(65536))
    report, ok = {}, True
    try:
        for offset in OFFSETS:
            resync()
            stamp = time.strftime('%m%d%H%M%Y.%S', time.gmtime(time.time() + offset))
            sudo('systemsetup -setusingnetworktime off >/dev/null 2>&1; date -u %s >/dev/null' % stamp)
            time.sleep(5)
            result = steps(target, offset, payload)
            report['%+d' % offset] = result
            ok = ok and all(result[k] for k in ('exec', 'ssh', 'send', 'terminal', 'doctor'))
    finally:
        resync()
        os.remove(payload)
    drift = abs(float(subprocess.run(['sh', '-c', 'sntp time.apple.com 2>/dev/null | awk \'{print $1}\''],
                                     capture_output=True, text=True).stdout.strip() or 99))
    report['clock_restored'] = drift < 2
    ok = ok and report['clock_restored']
    print(json.dumps({'check': 'clock-skew-mac', 'target': target, 'status': 'PASS' if ok else 'FAIL', **report}))
    return 0 if ok else 1


if __name__ == '__main__':
    sys.exit(main())
