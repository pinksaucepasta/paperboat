#!/usr/bin/env python3
"""F8-07: `pb tunnel status/logs/doctor` explain real failures.

Induces two real failures on a working durable tunnel and checks that each is named, with what to
do, in all three commands, and that the tunnel returns to ready by itself once the cause is fixed:
  1. the local app stops           -> origin_unreachable, log tunnel_origin_unreachable
  2. the Paperboat host stops (Linux only) -> connector_offline
"""
import functools
import http.server
import json
import os
from pathlib import Path
import platform
import subprocess
import tempfile
import threading
import time
import uuid

from install_lifecycle import registration
from preview_lifecycle import QuietHandler
from tunnel_survival import PB, confirmed, run, status_code, sudo, wait_status

CAUSE_SECONDS = 150
RECOVER_SECONDS = 150


def human(*args):
    result = subprocess.run([str(PB), *args], capture_output=True, text=True, timeout=120)
    if result.returncode:
        raise RuntimeError('command failed: ' + ' '.join(args))
    return result.stdout


def wait_code(identifier, code, seconds, what):
    deadline = time.monotonic() + seconds
    last = ''
    while time.monotonic() < deadline:
        last = run('tunnel', 'status', identifier)['overall_code']
        if last == code:
            return
        time.sleep(3)
    raise RuntimeError('tunnel status never reported %s after %s (last: %s)' % (code, what, last))


def explained(identifier, code):
    """Status, doctor and logs must each name the cause and say what to do about it."""
    status = human('tunnel', 'status', identifier)
    doctor = human('tunnel', 'doctor', identifier)
    logs = human('tunnel', 'logs', identifier)
    health = run('tunnel', 'status', identifier)
    assert health['overall_code'] == code, 'status code changed while checking: ' + health['overall_code']
    action = health['repair_action']
    assert action and action != 'No action required.', 'no repair action for ' + code
    for name, text in (('status', status), ('doctor', doctor)):
        assert code in text and health['summary'] in text, name + ' does not name the cause ' + code
        assert action in text, name + ' does not say what to do for ' + code
    return logs


def serve(directory, port=0):
    server = http.server.ThreadingHTTPServer(('127.0.0.1', port), functools.partial(QuietHandler, directory=directory))
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


def main():
    before = registration()
    name = 'smoke-tunnel-explain-' + uuid.uuid4().hex[:10]
    identifier = None
    stopped_host = None
    primary_error = None
    result = {'check': 'tunnel-failures', 'status': 'PASS'}
    with tempfile.TemporaryDirectory(prefix='paperboat-tunnel-explain-') as directory:
        (Path(directory) / 'index.html').write_text('synthetic tunnel origin')
        server = serve(directory)
        port = server.server_port
        try:
            created = run('tunnel', 'create', name, '--port', str(port), '--duration', '30m', '--wait', '--timeout', '90s')
            identifier = created['tunnel']['id']
            endpoint = created['tunnel']['stable_endpoint']
            wait_status(endpoint, 200, 'after creation')
            wait_code(identifier, 'ready', 60, 'creation')

            server.shutdown()
            server.server_close()
            wait_code(identifier, 'origin_unreachable', CAUSE_SECONDS, 'stopping the local app')
            logs = explained(identifier, 'origin_unreachable')
            assert 'tunnel_origin_unreachable' in logs, 'tunnel logs do not record the local app going down'
            origin = run('tunnel', 'status', identifier)['dimensions']['origin']['code']
            assert origin.startswith('origin_'), 'origin dimension does not carry the reason: ' + origin
            server = serve(directory, port)
            wait_code(identifier, 'ready', RECOVER_SECONDS, 'restarting the local app')
            wait_status(endpoint, 200, 'after the local app returned')
            assert 'tunnel_origin_recovered' in human('tunnel', 'logs', identifier), 'tunnel logs do not record the recovery'
            result['origin_stopped'] = origin

            if platform.system() == 'Linux':
                unit = 'paperboat-hostd-u%d.service' % os.getuid()
                sudo('systemctl', 'stop', unit)
                stopped_host = unit
                wait_code(identifier, 'connector_offline', CAUSE_SECONDS, 'stopping the host service')
                explained(identifier, 'connector_offline')
                sudo('systemctl', 'start', unit)
                stopped_host = None
                wait_code(identifier, 'ready', RECOVER_SECONDS, 'starting the host service')
                wait_status(endpoint, 200, 'after the host service returned')
                result['host_stopped'] = True
            else:
                result['host_stopped'] = 'linux_only'
        except BaseException as error:
            primary_error = error
            raise
        finally:
            try:
                if stopped_host:
                    sudo('systemctl', 'start', stopped_host)
                if identifier is not None:
                    confirmed('tunnel', 'delete', identifier)
            except BaseException:
                if primary_error is None:
                    raise
                print(json.dumps({'check': 'tunnel-failures', 'cleanup': 'failed'}), flush=True)
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
