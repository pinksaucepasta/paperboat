#!/usr/bin/env python3
"""F1-04/F1-08/F1-09, C011/C021, F2-04: uninstall, re-pair and interrupted setup on a throwaway container.

Creates a container paired to the member account (member_container.sh), runs the destructive checks inside it
(pairing_lifecycle flags/prompts/broken, with a SIGINT and a network fault during pair; setup_recovery; reset),
then revokes the machine and removes the container. Nothing touches a shared machine.
Usage: destructive_suite.py HOST BUILD_ON_HOST VERSION [CONTAINER=laneh-destructive]   env: ORIGIN STATE PW_CHROMIUM [PW_MODULE]
"""
import json
import os
import shlex
import subprocess
import sys
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
CHECKS = [
    'pairing_lifecycle.py --expect-version {v} --check flags --inventory uninstall_inventory.py',
    'pairing_lifecycle.py --expect-version {v} --check prompts --inventory uninstall_inventory.py',
    'pairing_lifecycle.py --expect-version {v} --check broken --inventory uninstall_inventory.py',
    'pairing_lifecycle.py --expect-version {v} --check flags --pair-fault sigint --inventory uninstall_inventory.py',
    'pairing_lifecycle.py --expect-version {v} --check flags --pair-fault network --inventory uninstall_inventory.py',
    'setup_recovery.py sigint',
    'setup_recovery.py network',
    'reset_lifecycle.py',
]
REVOKE_SELF = ('import json;from pathlib import Path;from pairing_lifecycle import confirmed_command,runtime_root;'
               'i=json.loads((runtime_root()/"machine-registration.json").read_text())["machine_id"];'
               'confirmed_command(Path.home()/".local/bin/pb","machine","revoke",i);print("revoked",i)')


def main():
    host, build, version = sys.argv[1:4]
    name = sys.argv[4] if len(sys.argv) > 4 else 'laneh-destructive'
    created = subprocess.run([str(HERE / 'member_container.sh'), host, name, build], capture_output=True, text=True)
    if 'True machine_' not in created.stdout:
        raise RuntimeError('could not create the throwaway container: ' + (created.stdout + created.stderr)[-300:])
    inside = ['ssh', host, 'docker', 'exec', '-i', '-u', 'tester', '-e', 'HOME=/home/tester', '-e', 'XDG_RUNTIME_DIR=/run/user/1001',
              '-w', '/home/tester/dsd', name]
    results = []
    try:
        for _ in range(24):  # a fresh pair needs a moment before it can reach itself
            if subprocess.run(inside + ['bash', '-lc', 'timeout 40 ~/.local/bin/pb exec %s -- true' % name], input='\n', capture_output=True, text=True).returncode == 0:
                break
            time.sleep(10)
        tarball = subprocess.run(['tar', 'czf', '-', '-C', str(HERE)] + sorted(p.name for p in HERE.glob('*.py')), capture_output=True, check=True).stdout
        subprocess.run(['ssh', host, 'docker', 'exec', '-i', '-u', 'tester', name, 'bash', '-c', shlex.quote('rm -rf ~/dsd && mkdir ~/dsd && tar xzf - -C ~/dsd')], input=tarball, check=True)
        for check in ([os.environ['ONLY']] if os.environ.get('ONLY') else CHECKS):  # ONLY: one check line, for debugging
            command = 'python3 ' + check.format(v=version)
            for attempt in (1, 2):  # a failure that does not repeat on one rerun is a flake (AGENTS.md triage); the retry is reported
                done = subprocess.run(inside + ['bash', '-c', shlex.quote(command)], input='\n', capture_output=True, text=True, timeout=900)
                if done.returncode == 0:
                    break
            last = [line for line in done.stdout.splitlines() if line.startswith('{')][-1:] or [(done.stdout + done.stderr)[-200:].replace('\n', ' / ')]
            results.append((' '.join(check.split()[:1] + [a for a in check.split()[1:] if not a.startswith('{')][:6]), done.returncode == 0, ('retried once: ' if attempt == 2 else '') + last[0][:300]))
    finally:
        if not os.environ.get('KEEP'):  # KEEP=1 leaves the container for debugging; remove it by hand
            subprocess.run(inside + ['bash', '-c', shlex.quote('python3 -c ' + shlex.quote(REVOKE_SELF))], capture_output=True, text=True, timeout=120)
            subprocess.run(['ssh', host, 'docker rm -f ' + name], capture_output=True)
    for label, ok, detail in results:
        print('PASS' if ok else 'FAIL', label, detail)
    return 0 if all(ok for _, ok, _ in results) else 1


if __name__ == '__main__':
    sys.exit(main())
