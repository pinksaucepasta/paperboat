#!/usr/bin/env python3
"""F10-03 / F4-04: an ENV delivery that fails says so (the specific message) and the command still runs.

  env_undeliverable.py TARGET --inject 'SHELL' --clear 'SHELL' [--wait 100]

Run on a controller machine; TARGET is another machine of the account that is safe to disturb (a
disposable machine, never a shared one). --inject must make the TARGET host runtime's new connections
to the control plane fail (the layer fetch of a launch then fails for a reason that is not "not ready
yet"); --clear undoes it. Example for a container on this machine whose host runtime is the cgroup
system.slice/paperboat-hostd-u1000.service:
  --inject "sudo docker exec CT iptables -A OUTPUT -p tcp --dport 443 -m cgroup --path CGROUP -j REJECT --reject-with tcp-reset"
  --clear  "sudo docker exec CT iptables -D OUTPUT -p tcp --dport 443 -m cgroup --path CGROUP -j REJECT --reject-with tcp-reset"
The check waits --wait seconds after injecting so idle kept-alive connections are gone.
  1. baseline: a command on TARGET runs and prints no ENV line
  2. injected: it still runs and says 'delivery to this machine failed' and points at `pb env host show`
  3. cleared: it runs again with no ENV line
"""
import argparse
import json
from pathlib import Path
import subprocess
import time

PB = Path.home() / '.local/bin/pb'
FAILED = ('delivery to this machine failed', 'pb env host show')


def run(*args):
    result = subprocess.run([str(PB), *args], capture_output=True, text=True, timeout=120)
    return result.returncode, result.stdout, result.stderr


def sh(command):
    result = subprocess.run(command, shell=True, capture_output=True, text=True, timeout=120)
    if result.returncode:
        raise RuntimeError('command failed: ' + command[:80] + ' ' + result.stderr[:120])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--inject', required=True)
    parser.add_argument('--clear', required=True)
    parser.add_argument('--wait', type=int, default=100)
    args = parser.parse_args()
    code, out, err = run('exec', args.target, '--', 'sh', '-c', 'echo baseline')
    if code or 'baseline' not in out or 'ENV not applied' in err:
        raise RuntimeError('the baseline command did not run cleanly: ' + err[:160])
    sh(args.inject)
    injected = None
    try:
        time.sleep(args.wait)
        code, out, err = run('exec', args.target, '--', 'sh', '-c', 'echo injected')
        injected = (code, out, err)
    finally:
        sh(args.clear)
    code, out, err = injected
    if code or 'injected' not in out:
        raise RuntimeError('the command did not run while delivery was failing (managed ENV must never stop a launch): ' + err[:160])
    if not all(word in err for word in FAILED):
        raise RuntimeError('the failed delivery was not named: ' + err[:200])
    code, out, err = run('exec', args.target, '--', 'sh', '-c', 'echo cleared')
    if code or 'cleared' not in out or 'ENV not applied' in err:
        raise RuntimeError('the command after clearing did not run cleanly: ' + err[:160])
    print(json.dumps({'check': 'env-undeliverable', 'target': args.target, 'status': 'PASS',
                      'command_ran': True, 'message': 'delivery to this machine failed'}), flush=True)


if __name__ == '__main__':
    main()
