#!/usr/bin/env python3
"""C022-C024 on a container whose account has a console session: runs auth_lifecycle.py there and approves its login link.

auth_lifecycle.py writes the browser approval link to a file and waits; this controller reads the link and approves it in
headless Chromium with the account's stored console session (STATE, mode 600, never printed).
Usage: auth_lifecycle_approved.py HOST CONTAINER USER [UID=1001] [MACHINE=CONTAINER]   env: STATE, PW_CHROMIUM, [PW_MODULE]
"""
import os
import shlex
import subprocess
import sys
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent


def main():
    host, container, user = sys.argv[1:4]
    uid = sys.argv[4] if len(sys.argv) > 4 else '1001'
    machine = sys.argv[5] if len(sys.argv) > 5 else container  # the machine name exec dials
    inside = 'ssh %s docker exec -i -u %s -e HOME=/home/%s -e XDG_RUNTIME_DIR=/run/user/%s %s ' % (host, user, user, uid, container)
    exec_in = lambda script: inside + shlex.quote('bash -c ' + shlex.quote(script))
    tarball = subprocess.run(['tar', 'czf', '-', '-C', str(HERE)] + sorted(p.name for p in HERE.glob('*.py')), capture_output=True, check=True).stdout
    subprocess.run(exec_in('rm -rf ~/dsa && mkdir ~/dsa && tar xzf - -C ~/dsa && rm -f /tmp/auth-url'), shell=True, input=tarball, check=True)
    job = subprocess.Popen(exec_in('cd ~/dsa && python3 auth_lifecycle.py %s /tmp/auth-url' % shlex.quote(machine)), shell=True,
                           stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    link = ''
    deadline = time.monotonic() + 240
    while time.monotonic() < deadline and job.poll() is None and not link.startswith('https://'):
        time.sleep(3)
        link = subprocess.run(exec_in('cat /tmp/auth-url 2>/dev/null'), shell=True, capture_output=True, text=True).stdout.strip()
    if link.startswith('https://'):
        subprocess.run(['node', str(HERE / 'approve_cli_login.mjs'), link], check=True, timeout=180, env=os.environ)
    out = job.communicate(timeout=300)[0]
    print(out.strip()[-600:])
    return job.returncode


if __name__ == '__main__':
    sys.exit(main())
