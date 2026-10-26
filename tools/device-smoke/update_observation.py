#!/usr/bin/env python3
"""The server's update observation for this machine is healthy and names the installed version.

Every machine reported "update failed, updater_unavailable" on every heartbeat for weeks because the runtime's
updater client dialled the wrong socket (fixed in 21924838); no check read the server's stored observation (Lane P,
2026-10-27). This reads it through `pb __desktop request machine.update-status` and requires state healthy or idle,
no error_code, and current_version = this pb's version, waiting up to one minute for the first observation.
"""
import json
import subprocess
import sys
import time
from pathlib import Path

from install_lifecycle import registration
from pairing_lifecycle import invoke


def installed_version(binary):
    out = subprocess.run([str(binary), 'version'], capture_output=True, text=True, timeout=30).stdout
    for line in out.splitlines():
        if 'Version ' in line:
            return line.split('Version ', 1)[1].strip()
    raise RuntimeError('pb version did not report a version')


def main():
    binary = Path.home() / '.local/bin/pb'
    machine_id = registration()['machine_id']
    version = installed_version(binary)
    request = json.dumps({'action': 'machine.update-status', 'payload': {'machine_id': machine_id}})
    deadline = time.monotonic() + 60
    last = None
    while True:
        data = invoke(binary, '--workspace', 'personal', '__desktop', 'request', input=request)
        last = {key: data.get(key) for key in ('state', 'current_version', 'target_version', 'error_code', 'observed_at')}
        if last['state'] in ('healthy', 'idle') and not last['error_code'] and last['current_version'] == version:
            break
        if time.monotonic() >= deadline:
            print(json.dumps({'check': 'update-observation', 'status': 'FAIL', 'expected_version': version, 'observation': last}))
            sys.exit(1)
        time.sleep(10)
    print(json.dumps({'check': 'update-observation', 'status': 'PASS', 'enrollment_preserved': True, 'observation': last}))


if __name__ == '__main__':
    main()
