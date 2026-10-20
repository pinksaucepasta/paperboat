#!/usr/bin/env python3
"""F10-03 / F10-05: what a command says about managed ENV delivery, and that `pb env host show` agrees.

  env_delivery.py TARGET --expect empty|applied|pending|undeliverable

The command always runs (exit 0): managed ENV is an addition to a launch, never a precondition.
  empty         no ENV assigned: host show lists no layers, no ENV line.
  applied       delivered: host show says published and applied, no ENV line.
  pending       not delivered yet: host show says not applied; the command says delivery is pending
                and points at `pb env vault unlock`; the value is absent.
  undeliverable delivery failed: the command says so and points at `pb env host show`.
The caller creates the state (for pending: change a scope while no unlocked device can re-seal it).
"""
import argparse
import json
from pathlib import Path
import subprocess

PB = Path.home() / '.local/bin/pb'
SENTINEL = 'PB_ENV_DELIVERY_PROBE'
PENDING = ('delivery to this machine is pending', 'pb env vault unlock')
UNDELIVERABLE = ('delivery to this machine failed', 'pb env host show')


def run(*args, timeout=90):
    result = subprocess.run([str(PB), *args], capture_output=True, text=True, timeout=timeout)
    return result.returncode, result.stdout, result.stderr


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--expect', required=True, choices=['empty', 'applied', 'pending', 'undeliverable'])
    args = parser.parse_args()
    code, out, err = run('env', 'host', 'show', args.target, '--json')
    if code:
        raise RuntimeError('env host show failed')
    host = json.loads(out)
    code, out, err = run('exec', args.target, '--', 'sh', '-c', 'echo "[$%s]"' % SENTINEL)
    if code:
        raise RuntimeError('the command did not run (exit %d); managed ENV must never stop a launch' % code)
    has_line = 'ENV not applied' in err
    if args.expect in ('empty', 'applied') and has_line:
        raise RuntimeError('an ENV warning was shown although delivery is not failing')
    if args.expect == 'empty' and host['layers']:
        raise RuntimeError('host show lists layers for an empty ENV')
    if args.expect == 'applied' and not (host['published'] and host['applied']):
        raise RuntimeError('host show does not report the delivery as published and applied')
    if args.expect in ('pending', 'undeliverable'):
        words = PENDING if args.expect == 'pending' else UNDELIVERABLE
        if not all(word in err for word in words):
            raise RuntimeError('the %s message is missing or generic' % args.expect)
        if args.expect == 'pending' and host['applied']:
            raise RuntimeError('host show reports applied while the command says delivery is pending')
        if out.strip() != '[]':
            raise RuntimeError('the probe value was present although delivery did not complete')
    print(json.dumps({'check': 'env-delivery', 'target': args.target, 'expect': args.expect, 'status': 'PASS',
                      'published': host['published'], 'applied': host['applied']}), flush=True)


if __name__ == '__main__':
    main()
