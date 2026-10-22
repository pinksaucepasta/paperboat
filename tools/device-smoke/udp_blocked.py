#!/usr/bin/env python3
"""Exec to a target whose network blocks UDP: the edge carrier must fall back to HTTP/2 over TCP.

Blocks outbound UDP on the target except DNS and 443 (an office/hotel firewall; Lane O's rule),
restarts its carrier so it dials again, and requires `pb exec <target> -- true` to work within
the budget. The block is removed at the end and also by a systemd timer after four minutes, so a
failed fallback never strands the machine. Needs passwordless sudo and nft on the target (the
disposable test containers). Prints one JSON receipt; no command output is retained.
"""
import argparse
import json
from pathlib import Path
import subprocess
import time

from install_lifecycle import registration

PB = Path.home() / '.local/bin/pb'
TABLE = 'pbsmoke_udp'
RECOVERY_SECONDS = 90
BLOCK = (
    f'table inet {TABLE} {{ chain out {{ type filter hook output priority 0; '
    'udp dport { 53, 443 } accept; ip protocol udp drop; ip6 nexthdr udp drop; } }')


def pb_exec(target, *command, timeout=60):
    result = subprocess.run([str(PB), 'exec', target, '--timeout', '45s', '--', *command],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                            stdin=subprocess.DEVNULL, timeout=timeout)
    return result.returncode == 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    before = registration()
    if not pb_exec(args.target, 'true'):
        raise RuntimeError('target is not reachable before UDP is blocked')
    script = (f'nft delete table inet {TABLE} 2>/dev/null; echo {json.dumps(BLOCK)} | nft -f - && '
              f'systemd-run --quiet --on-active=240 nft delete table inet {TABLE} && '
              '(systemctl restart "paperboat-carrier-u$(id -u "$SUDO_USER").service" || true)')
    # The block cuts the session that applies it; the command's own result is not meaningful.
    pb_exec(args.target, 'sudo', '-n', 'sh', '-c', script)
    started, recovered = time.monotonic(), None
    try:
        while time.monotonic() - started < RECOVERY_SECONDS:
            if pb_exec(args.target, 'sh', '-c', f'sudo -n nft list table inet {TABLE} >/dev/null'):
                recovered = time.monotonic() - started
                break
            time.sleep(3)
    finally:
        removed = pb_exec(args.target, 'sudo', '-n', 'nft', 'delete', 'table', 'inet', TABLE)
    print(json.dumps({'status': 'PASS' if recovered is not None and removed else 'FAIL',
                      'target': args.target, 'check': 'udp-blocked',
                      'recovery_seconds': None if recovered is None else round(recovered, 1),
                      'block_removed': removed, 'enrollment_unchanged': registration() == before}))


if __name__ == '__main__':
    main()
