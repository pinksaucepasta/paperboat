#!/usr/bin/env python3
"""F14-03: failed updates (bad file, truncated file, wrong platform, killed mid-update) keep a working install.

Usage: update_failures.py TARGET GOOD_BINARY WRONG_PLATFORM_BINARY (sudo password on stdin).
After each failure the installed version is unchanged, `pb doctor` is healthy and exec works;
a killed update ends with either the old or the new version, never a broken runtime.
"""
import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time

import pairing_lifecycle as p
import terminal_command

PB = Path.home() / '.local/bin/pb'


def version():
    return json.loads(p.invoke(PB, '--version', '--json', raw=True))['data']['version']


def healthy(target, seconds=150):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            if (p.invoke(PB, 'doctor', '--json', timeout=60)['overall'] == 'healthy'
                    and p.command_result(PB, 'exec', target, '--', 'true', timeout=60).returncode == 0):
                return True
        except Exception:
            pass
        time.sleep(4)
    return False


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('good')
    parser.add_argument('wrong_platform')
    args = parser.parse_args()
    password = sys.stdin.readline().rstrip('\n')
    original = version()
    refused = []
    with tempfile.TemporaryDirectory(prefix='paperboat-update-fail-') as directory:
        base = Path(directory)
        good = Path(args.good).read_bytes()
        files = {'garbage': os.urandom(4096), 'truncated': good[:len(good) // 3],
                 'wrong_platform': Path(args.wrong_platform).read_bytes()}
        for name, content in files.items():
            candidate = base / name
            candidate.write_bytes(content)
            candidate.chmod(0o700)
            result = p.command_result(PB, 'update', '--from', str(candidate), '--json',
                                      timeout=300, terminal=True, password=password)
            if result.returncode == 0:
                raise RuntimeError(name + ' update was accepted')
            if version() != original or not healthy(args.target):
                raise RuntimeError(name + ' update left the install changed or unhealthy')
            refused.append(name)
        # Killed mid-update: the previous or the new version must work afterwards.
        candidate = base / 'good'
        candidate.write_bytes(good)
        candidate.chmod(0o700)
        child = subprocess.Popen([sys.executable, '-c',
                                  'import sys,terminal_command as t;'
                                  't.run([sys.argv[1],"update","--from",sys.argv[2],"--force","--json"],'
                                  'password=sys.argv[3],timeout=300)', str(PB), str(candidate), password],
                                 start_new_session=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        time.sleep(3)
        os.killpg(child.pid, signal.SIGKILL)
        child.wait()
        if not healthy(args.target, 240):
            raise RuntimeError('install is not healthy after a killed update')
        after_kill = version()
        if after_kill not in (original, json.loads(subprocess.run([str(candidate), '--version', '--json'],
                                                                  capture_output=True, text=True).stdout)['data']['version']):
            raise RuntimeError('killed update left an unexpected version')
        # Finish or retry cleanly so the next row starts from a known state.
        p.invoke(PB, 'update', '--from', str(candidate), '--json', raw=True, terminal=True, password=password, timeout=300)
        if not healthy(args.target, 240):
            raise RuntimeError('install is not healthy after the retried update')
    print(json.dumps({'check': 'update-failures', 'target': args.target, 'status': 'PASS',
                      'refused': refused, 'killed_update_recovers': True, 'previous_version': original,
                      'version_after_kill': after_kill}), flush=True)


if __name__ == '__main__':
    main()
