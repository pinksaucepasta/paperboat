#!/usr/bin/env python3
"""F7-04: a background preview survives a daemon restart and reports a stopped app, then recovers.

  1. background preview serves the marker
  2. `pb service restart` (the user daemon): the same URL serves again and the lease stays ready
  3. the local app stops: the lease becomes origin_unavailable and the URL stops serving it
  4. the app returns on the same port: the lease is ready again and the URL serves the marker
  5. `preview stop` ends it: state stopped and the URL no longer serves
Also records (does not assert) what a host-service restart does to the preview.
"""
import functools
import http.server
import json
import os
from pathlib import Path
import platform
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
from tunnel_survival import sudo

PB = Path.home() / '.local/bin/pb'
WAIT_SECONDS = 150


def body(endpoint):
    try:
        return urllib.request.urlopen(endpoint + '/', timeout=15).read().decode('utf-8', 'replace')
    except urllib.error.HTTPError as error:
        return 'HTTP %d' % error.code
    except OSError:
        return ''


def wait_for(check, what):
    deadline = time.monotonic() + WAIT_SECONDS
    while time.monotonic() < deadline:
        if check():
            return
        time.sleep(3)
    raise RuntimeError('timed out waiting for ' + what)


def state(identifier):
    return invoke(PB, 'preview', 'status', identifier, '--json', timeout=30)['state']


def serve(directory, port=0):
    server = http.server.ThreadingHTTPServer(('127.0.0.1', port), functools.partial(QuietHandler, directory=directory))
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


def main():
    before = registration()
    marker = 'PB_PREVIEW_SURVIVE_' + uuid.uuid4().hex
    identifier = None
    primary_error = None
    result = {'check': 'preview-survival', 'status': 'PASS'}
    with tempfile.TemporaryDirectory(prefix='paperboat-preview-survive-') as directory:
        (Path(directory) / 'index.html').write_text('<p>' + marker + '</p>')
        server = serve(directory)
        port = server.server_port
        try:
            lease = invoke(PB, 'preview', str(port), '--background', '--ttl', '15m', '--json', timeout=PREVIEW_COMMAND_TIMEOUT)
            endpoint = receipt_value(lease, 'endpoint')
            identifier = lease['id']
            wait_for(lambda: marker in body(endpoint), 'the preview to serve its marker')

            invoke(PB, 'service', 'restart', '--json', timeout=180)
            time.sleep(3)
            wait_for(lambda: marker in body(endpoint) and state(identifier) == 'ready', 'the same URL after a daemon restart')
            result['daemon_restart'] = True

            server.shutdown()
            server.server_close()
            wait_for(lambda: state(identifier) == 'origin_unavailable', 'origin_unavailable after the app stopped')
            assert marker not in body(endpoint), 'the preview kept serving a stopped app'
            result['origin_unavailable'] = True

            server = serve(directory, port)
            wait_for(lambda: state(identifier) == 'ready' and marker in body(endpoint), 'ready again after the app returned')
            result['origin_recovers'] = True

            stopped = invoke(PB, 'preview', 'stop', identifier, '--json', timeout=60)
            receipt_value(stopped, 'stopped', identifier)
            identifier_stopped, identifier = identifier, None
            assert state(identifier_stopped) == 'stopped', 'stop did not persist the stopped state'
            wait_for(lambda: marker not in body(endpoint), 'the stopped preview to stop serving')
            result['stop_ends'] = True
            result['host_restart'] = 'not_run'
            if platform.system() == 'Linux':
                lease = invoke(PB, 'preview', str(port), '--background', '--ttl', '15m', '--json', timeout=PREVIEW_COMMAND_TIMEOUT)
                identifier, endpoint = lease['id'], receipt_value(lease, 'endpoint')
                wait_for(lambda: marker in body(endpoint), 'the second preview to serve')
                sudo('systemctl', 'restart', 'paperboat-hostd-u%d.service' % os.getuid())
                time.sleep(20)
                result['host_restart'] = state(identifier)
        except BaseException as error:
            primary_error = error
            raise
        finally:
            try:
                if identifier is not None:
                    invoke(PB, 'preview', 'stop', identifier, '--json', timeout=60)
            except BaseException:
                if primary_error is None:
                    raise
                print(json.dumps({'check': 'preview-survival', 'cleanup': 'failed'}), flush=True)
            finally:
                try:
                    server.shutdown()
                    server.server_close()
                except Exception:
                    pass
    assert registration() == before
    result['enrollment_preserved'] = True
    print(json.dumps(result))


if __name__ == '__main__':
    main()
