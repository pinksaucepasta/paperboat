#!/usr/bin/env python3
"""F7-03 (refusal side): --private and --team previews refuse a visitor with no session; --public serves."""
import argparse
import functools
import http.server
import json
from pathlib import Path
import tempfile
import threading
import time
import urllib.error
import urllib.request
import uuid

from install_lifecycle import registration
from pairing_lifecycle import invoke
from preview_lifecycle import QuietHandler
from preview_receipt import PREVIEW_COMMAND_TIMEOUT, receipt_value

TEAM_WORKSPACE = 'paperboat-smoke-paid'


def visit(url):
    """Status and body prefix of an unauthenticated HTTPS request, retried while the name is still arriving."""
    last = None
    for _ in range(20):
        try:
            with urllib.request.urlopen(url, timeout=10) as response:
                return response.status, response.read(200).decode(errors='replace')
        except urllib.error.HTTPError as error:
            return error.code, error.read(200).decode(errors='replace')
        except (urllib.error.URLError, OSError) as error:
            last = error
            time.sleep(2)
    raise RuntimeError('preview did not answer: ' + type(last).__name__)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    binary = Path.home() / '.local/bin/pb'
    before = registration()
    marker = 'PB_ACCESS_' + uuid.uuid4().hex
    owned = []
    failure = None
    with tempfile.TemporaryDirectory(prefix='paperboat-preview-access-') as directory:
        (Path(directory) / 'index.html').write_text(marker)
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), functools.partial(QuietHandler, directory=directory))
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        results = {}
        try:
            for mode, flags in [('public', []), ('private', ['--private']),
                                ('team', ['--team', '--workspace', TEAM_WORKSPACE])]:
                lease = invoke(binary, 'preview', str(server.server_port), '--background', '--ttl', '10m',
                               *flags, '--json', timeout=PREVIEW_COMMAND_TIMEOUT)
                receipt_value(lease, 'endpoint')
                owned.append((lease['id'], flags))
                if lease['access_mode'] != mode:
                    raise RuntimeError('preview access mode is %s, not %s' % (lease['access_mode'], mode))
                status, body = visit(lease['endpoint'])
                if mode == 'public':
                    # The name is still reaching resolvers: wait until it serves, then judge.
                    deadline = time.monotonic() + 60
                    while (status != 200 or marker not in body) and time.monotonic() < deadline:
                        time.sleep(2)
                        status, body = visit(lease['endpoint'])
                results[mode] = status
                if mode == 'public' and (status != 200 or marker not in body):
                    raise RuntimeError('the public preview did not serve its page to a stranger (HTTP %d)' % status)
                if mode != 'public' and (status not in (401, 403, 404) or marker in body):
                    raise RuntimeError('the %s preview did not refuse a visitor with no session (HTTP %d)' % (mode, status))
        except Exception as error:
            failure = error
            raise
        finally:
            try:
                for identity, flags in owned:
                    workspace = flags[-2:] if '--workspace' in flags else []
                    invoke(binary, *workspace, 'preview', 'stop', identity, '--json', timeout=30)
            except Exception as error:
                if failure is None:
                    raise
                failure.add_note('Exact preview cleanup failed: ' + str(error))
            finally:
                server.shutdown()
                server.server_close()
                thread.join(timeout=5)
    if registration() != before:
        raise RuntimeError('preview access check changed the enrolled identity')
    print(json.dumps({'check': 'preview-access', 'target': args.target, 'status': 'PASS',
                      'public_serves': True, 'private_refuses_stranger': True, 'team_refuses_stranger': True,
                      'statuses': results, 'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
