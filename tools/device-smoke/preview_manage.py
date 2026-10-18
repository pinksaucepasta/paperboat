#!/usr/bin/env python3
"""Verify preview list/status, exact stop/delete, and the declared TTL."""
import argparse
import functools
import http.server
import json
from pathlib import Path
import tempfile
import threading
import time

from install_lifecycle import registration
from pairing_lifecycle import invoke
from preview_lifecycle import QuietHandler
from preview_receipt import PREVIEW_COMMAND_TIMEOUT, receipt_timestamp, receipt_value


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    binary = Path.home() / '.local/bin/pb'
    before = registration()
    owned = set()
    failure = None
    with tempfile.TemporaryDirectory(prefix='paperboat-preview-manage-') as directory:
        (Path(directory) / 'index.html').write_text('Paperboat synthetic preview fixture')
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), functools.partial(QuietHandler, directory=directory))
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()

        def create(ttl):
            lease = invoke(binary, 'preview', str(server.server_port), '--background', '--ttl', ttl, '--json', timeout=PREVIEW_COMMAND_TIMEOUT)
            receipt_value(lease, 'endpoint')
            owned.add(lease['id'])
            return lease

        try:
            for command in ['stop', 'delete']:
                lease = create('5m')
                identity = lease['id']
                status = invoke(binary, 'preview', 'status', identity, '--json', timeout=20)
                if status['id'] != identity or status['state'] != 'ready':
                    raise RuntimeError('preview status did not identify the ready resource')
                page = invoke(binary, 'preview', 'list', '--q', identity, '--json', timeout=20)
                if [item['id'] for item in page['items']] != [identity]:
                    raise RuntimeError('filtered preview list did not retain its exact resource')
                stopped = invoke(binary, 'preview', command, identity, '--json', timeout=20)
                receipt_value(stopped, 'stopped', identity)
                status = invoke(binary, 'preview', 'status', identity, '--json', timeout=20)
                if status['state'] != 'stopped':
                    raise RuntimeError('preview stop/delete did not persist its documented stopped state')
                owned.remove(identity)
            lease = create('30s')
            deadline = receipt_timestamp(lease['user_deadline']).timestamp()
            if deadline - time.time() > 30:
                raise RuntimeError('preview extended its requested TTL')
            while time.time() < deadline + 30:
                status = invoke(binary, 'preview', 'status', lease['id'], '--json', timeout=20)
                if status['state'] == 'expired':
                    owned.remove(lease['id'])
                    break
                time.sleep(1)
            else:
                raise RuntimeError('preview did not expire at its declared TTL')
        except Exception as error:
            failure = error
            raise
        finally:
            try:
                for identity in owned:
                    invoke(binary, 'preview', 'stop', identity, '--json', timeout=20)
            except Exception as error:
                if failure is None:
                    raise
                failure.add_note('Exact preview cleanup failed: ' + str(error))
            finally:
                server.shutdown()
                server.server_close()
                thread.join(timeout=5)
    if registration() != before:
        raise RuntimeError('preview management changed the enrolled identity')
    print(json.dumps({'check': 'preview-manage', 'target': args.target, 'status': 'PASS',
                      'phases': ['list', 'status', 'stop', 'delete', 'ttl'], 'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
