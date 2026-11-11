#!/usr/bin/env python3
"""F20 gate (PLAN 3a, edge restart): back-to-back preview creates all succeed while one edge restarts.
Run it, then restart one edge of the machine's pair while it runs. Each create must return a lease
whose URL serves the marker; every preview is stopped at the end. Prints one JSON receipt.
  edge_restart_previews.py [--count 20] [--interval 1.5] [--pin-edge-ip IP --restart-done-file PATH]
With --pin-edge-ip the check also proves the restarted edge serves again: DNS returns both edges, so
after the creates it waits for --restart-done-file (touched by the operator once the restart returned),
then requests every preview with curl --resolve pinned to that one edge IP until each serves the marker.
"""
import argparse
import json
import subprocess
import tempfile
import time
import uuid
from pathlib import Path
from urllib.parse import urlsplit

from pairing_lifecycle import invoke
from preview_receipt import PREVIEW_COMMAND_TIMEOUT, receipt_value
from preview_lifecycle import stoppable_origin
from preview_survival import PB, body, wait_for


def pinned_body(endpoint, edge_ip):
    """Body of endpoint fetched from one edge IP only (curl --resolve); '' on any failure."""
    url = urlsplit(endpoint)
    port = url.port or 443
    result = subprocess.run(['curl', '-sS', '--max-time', '15', '--resolve', '%s:%d:%s' % (url.hostname, port, edge_ip), endpoint],
                            capture_output=True, text=True, timeout=30)
    return result.stdout if result.returncode == 0 else ''


def pin_every_preview(endpoints, marker, edge_ip, done_file, wait_s=600, serve_s=180):
    """Each preview must serve from edge_ip after the restart; returns (served seconds per index, failed indexes)."""
    deadline = time.monotonic() + wait_s
    while not Path(done_file).exists():
        if time.monotonic() > deadline:
            return {}, list(range(len(endpoints)))
        time.sleep(2)
    served, failed = {}, []
    for index, endpoint in enumerate(endpoints):
        started = time.monotonic()
        while marker not in pinned_body(endpoint, edge_ip):
            if time.monotonic() - started > serve_s:
                failed.append(index)
                break
            time.sleep(3)
        else:
            served[index] = round(time.monotonic() - started, 1)
    return served, failed


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--count', type=int, default=20)
    parser.add_argument('--interval', type=float, default=1.5)
    parser.add_argument('--pin-edge-ip')
    parser.add_argument('--restart-done-file')
    args = parser.parse_args()
    if bool(args.pin_edge_ip) != bool(args.restart_done_file):
        parser.error('--pin-edge-ip and --restart-done-file go together')
    endpoints, pinned, pin_failed = [], {}, []
    marker = 'PB_EDGE_RESTART_' + uuid.uuid4().hex
    created, failures = [], []
    with tempfile.TemporaryDirectory(prefix='paperboat-edge-restart-') as directory:
        (Path(directory) / 'index.html').write_text('<p>' + marker + '</p>')
        port, stop_origin = stoppable_origin(directory, '<p>' + marker + '</p>')
        try:
            for attempt in range(args.count):
                started = time.monotonic()
                try:
                    lease = invoke(PB, 'preview', str(port), '--background', '--ttl', '10m', '--json', timeout=PREVIEW_COMMAND_TIMEOUT)
                    created.append(lease['id'])
                    endpoint = receipt_value(lease, 'endpoint')
                    endpoints.append(endpoint)
                    wait_for(lambda: marker in body(endpoint), 'preview %d to serve' % attempt)
                except Exception as error:  # one receipt line per failed create, then keep going
                    failures.append({'attempt': attempt, 'after_s': round(time.monotonic() - started, 1), 'error': type(error).__name__})
                time.sleep(args.interval)
            if args.pin_edge_ip:
                pinned, pin_failed = pin_every_preview(endpoints, marker, args.pin_edge_ip, args.restart_done_file)
        finally:
            for identifier in created:
                try:
                    invoke(PB, 'preview', 'stop', identifier, '--json', timeout=60)
                except Exception:
                    pass
            stop_origin()
    ok = not failures and len(created) == args.count and not pin_failed
    if args.pin_edge_ip:
        ok = ok and len(pinned) == len(created)
    print(json.dumps({'check': 'edge-restart-previews', 'count': args.count, 'created': len(created), 'failures': failures,
                      'pinned_edge_ip': args.pin_edge_ip, 'pinned_serve_s': pinned, 'pin_failed': pin_failed,
                      'status': 'PASS' if ok else 'FAIL'}))
    return 0 if ok else 1


if __name__ == '__main__':
    raise SystemExit(main())
