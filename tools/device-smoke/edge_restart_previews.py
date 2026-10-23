#!/usr/bin/env python3
"""F20 gate (PLAN 3a, edge restart): back-to-back preview creates all succeed while one edge restarts.
Run it, then restart one edge of the machine's pair while it runs. Each create must return a lease
whose URL serves the marker; every preview is stopped at the end. Prints one JSON receipt.
  edge_restart_previews.py [--count 20] [--interval 1.5]
"""
import argparse
import json
import tempfile
import time
import uuid
from pathlib import Path

from pairing_lifecycle import invoke
from preview_receipt import PREVIEW_COMMAND_TIMEOUT, receipt_value
from preview_survival import PB, body, serve, wait_for


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--count', type=int, default=20)
    parser.add_argument('--interval', type=float, default=1.5)
    args = parser.parse_args()
    marker = 'PB_EDGE_RESTART_' + uuid.uuid4().hex
    created, failures = [], []
    with tempfile.TemporaryDirectory(prefix='paperboat-edge-restart-') as directory:
        (Path(directory) / 'index.html').write_text('<p>' + marker + '</p>')
        server = serve(directory)
        try:
            for attempt in range(args.count):
                started = time.monotonic()
                try:
                    lease = invoke(PB, 'preview', str(server.server_port), '--background', '--ttl', '10m', '--json', timeout=PREVIEW_COMMAND_TIMEOUT)
                    created.append(lease['id'])
                    endpoint = receipt_value(lease, 'endpoint')
                    wait_for(lambda: marker in body(endpoint), 'preview %d to serve' % attempt)
                except Exception as error:  # one receipt line per failed create, then keep going
                    failures.append({'attempt': attempt, 'after_s': round(time.monotonic() - started, 1), 'error': type(error).__name__})
                time.sleep(args.interval)
        finally:
            for identifier in created:
                try:
                    invoke(PB, 'preview', 'stop', identifier, '--json', timeout=60)
                except Exception:
                    pass
            server.shutdown()
    print(json.dumps({'check': 'edge-restart-previews', 'count': args.count, 'created': len(created), 'failures': failures,
                      'status': 'PASS' if not failures and len(created) == args.count else 'FAIL'}))
    return 0 if not failures else 1


if __name__ == '__main__':
    raise SystemExit(main())
