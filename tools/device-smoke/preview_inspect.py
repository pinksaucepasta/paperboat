#!/usr/bin/env python3
"""Verify preview HTTP capture: enable, list, show, deliberate replay, purge and disable."""
import argparse
import http.server
import json
from pathlib import Path
import threading
import urllib.request
import uuid

from install_lifecycle import registration
from pairing_lifecycle import invoke
from preview_receipt import PREVIEW_COMMAND_TIMEOUT, receipt_value


class Origin(http.server.BaseHTTPRequestHandler):
    seen = []

    def log_message(self, *args):
        pass

    def respond(self):
        length = int(self.headers.get('Content-Length') or 0)
        body = self.rfile.read(length).decode() if length else ''
        type(self).seen.append((self.command, self.path, body))
        data = b'captured'
        self.send_response(200)
        self.send_header('Content-Length', str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    do_GET = do_POST = respond


def fetch(url, method='GET', data=None):
    request = urllib.request.Request(url, data=data, method=method, headers={'Content-Type': 'application/json'})
    with urllib.request.urlopen(request, timeout=30) as response:
        return response.status, response.read()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    binary = Path.home() / '.local/bin/pb'
    before = registration()
    server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Origin)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    identity = None
    failure = None
    try:
        lease = invoke(binary, 'preview', str(server.server_port), '--background', '--ttl', '10m', '--json',
                       timeout=PREVIEW_COMMAND_TIMEOUT)
        receipt_value(lease, 'endpoint')
        identity = lease['id']
        url = lease['endpoint']

        invoke(binary, 'preview', 'inspect', identity, '--enable', '--body', '--raw', '--json', timeout=30)

        secret = 'PB_INSPECT_' + uuid.uuid4().hex
        body = json.dumps({'note': secret})
        if fetch(url + '/hello?x=1')[0] != 200 or fetch(url + '/submit', 'POST', body.encode())[0] != 200:
            raise RuntimeError('the origin did not answer through the preview')
        page = invoke(binary, 'preview', 'inspect', identity, '--json', timeout=30)
        post = [r for r in page['records'] if r['method'] == 'POST' and '/submit' in r['url']]
        get = [r for r in page['records'] if r['method'] == 'GET' and '/hello' in r['url']]
        if len(post) != 1 or len(get) != 1 or post[0]['response_status'] != 200 or get[0]['response_status'] != 200:
            seen = [(r['method'], r['url'].split('?')[0][-12:], r['response_status'], r['state']) for r in page['records']]
            raise RuntimeError('capture list did not show the GET and POST with their statuses: ' + repr(seen))
        record = invoke(binary, 'preview', 'inspect', identity, '--id', post[0]['id'], '--json', timeout=30)
        if record['id'] != post[0]['id'] or secret not in record['request_body']:
            raise RuntimeError('capture show did not return the retained request body')

        Origin.seen.clear()
        invoke(binary, 'preview', 'replay', identity, post[0]['id'], '--json', timeout=60)
        if Origin.seen != [('POST', '/submit', body)]:
            raise RuntimeError('replay did not repeat exactly the retained request at the origin')

        purged = invoke(binary, 'preview', 'inspect', identity, '--purge', '--json', timeout=30)
        if purged.get('purged') != 'true':
            raise RuntimeError('purge was not acknowledged')
        # A purged resource has no capture policy: enable again before reading.
        invoke(binary, 'preview', 'inspect', identity, '--enable', '--json', timeout=30)
        if invoke(binary, 'preview', 'inspect', identity, '--json', timeout=30)['records']:
            raise RuntimeError('purge left captures behind')
        invoke(binary, 'preview', 'inspect', identity, '--disable', '--json', timeout=30)
        fetch(url + '/after-disable')
        invoke(binary, 'preview', 'inspect', identity, '--enable', '--json', timeout=30)
        if invoke(binary, 'preview', 'inspect', identity, '--json', timeout=30)['records']:
            raise RuntimeError('a disabled preview kept capturing')
    except Exception as error:
        failure = error
        raise
    finally:
        try:
            if identity:
                invoke(binary, 'preview', 'stop', identity, '--json', timeout=30)
        except Exception as error:
            if failure is None:
                raise
            failure.add_note('Exact preview cleanup failed: ' + str(error))
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)
    if registration() != before:
        raise RuntimeError('preview inspection changed the enrolled identity')
    print(json.dumps({'check': 'preview-inspect', 'target': args.target, 'status': 'PASS',
                      'phases': ['enable', 'list', 'show', 'replay', 'purge', 'disable'],
                      'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
