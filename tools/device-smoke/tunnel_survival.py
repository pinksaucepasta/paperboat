#!/usr/bin/env python3
"""F8-03: a durable tunnel keeps serving through pause/resume, a daemon restart, an update and a host-service restart."""
import functools
import http.server
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
import uuid

from install_lifecycle import registration
from pairing_lifecycle import command_result
from preview_lifecycle import QuietHandler

PB = Path.home() / '.local/bin/pb'


def run(*args, expect_ok=True):
    result = command_result(PB, *args, '--json', timeout=240)
    value = json.loads(result.stdout)
    if expect_ok and (result.returncode or (isinstance(value, dict) and value.get('ok') is False)):
        raise RuntimeError('command failed: ' + ' '.join(args))
    return value.get('data', value) if isinstance(value, dict) and 'ok' in value else value


def confirmed(*args):
    token = run(*args, expect_ok=False).get('confirmation_token')
    assert token, 'no confirmation preview for ' + ' '.join(args)
    return run(*args, '--confirm', token)


def status_code(endpoint):
    try:
        return urllib.request.urlopen(endpoint + '/', timeout=15).status
    except urllib.error.HTTPError as error:
        return error.code
    except OSError:
        return 0


def wait_status(endpoint, wanted, what, seconds=120):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if status_code(endpoint) == wanted:
            return
        time.sleep(2)
    raise RuntimeError('tunnel did not return ' + str(wanted) + ' ' + what)


def sudo(*args):
    subprocess.run(['sudo', '-n', *args], check=True, timeout=180)


def survive(endpoint, identifier, what, action):
    action()
    time.sleep(3)
    wait_status(endpoint, 200, 'after ' + what)
    # The first edge serving does not make the tunnel ready: status waits for the other edge too (measured
    # 2-3 s later after a host-service restart), so give it a bounded window instead of one read.
    deadline = time.monotonic() + 30
    while run('tunnel', 'status', identifier)['overall_code'] != 'ready':
        assert time.monotonic() < deadline, 'not ready 30 s after ' + what
        time.sleep(1)


def main():
    before = registration()
    name = 'smoke-tunnel-survive-' + uuid.uuid4().hex[:10]
    identifier = None
    primary_error = None
    with tempfile.TemporaryDirectory(prefix='paperboat-tunnel-survive-') as directory:
        (Path(directory) / 'index.html').write_text('synthetic tunnel origin')
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0),
                    functools.partial(QuietHandler, directory=directory))
        worker = threading.Thread(target=server.serve_forever, daemon=True)
        worker.start()
        try:
            created = run('tunnel', 'create', name, '--port', str(server.server_port), '--duration', '30m',
                          '--wait', '--timeout', '90s')
            identifier = created['tunnel']['id']
            endpoint = created['tunnel']['stable_endpoint']
            wait_status(endpoint, 200, 'after creation')
            run('tunnel', 'pause', identifier, '--wait')
            assert status_code(endpoint) != 200, 'a paused tunnel still served traffic'
            run('tunnel', 'resume', identifier, '--wait')
            wait_status(endpoint, 200, 'after resume')
            survive(endpoint, identifier, 'a daemon restart', lambda: run('service', 'restart'))
            installed = Path(tempfile.mkdtemp(prefix='pb-survive-', dir=Path.home()))
            try:
                os.chmod(installed, 0o700)
                copy = installed / 'pb'
                shutil.copy2(PB, copy)
                os.chmod(copy, 0o700)
                survive(endpoint, identifier, 'an update', lambda: subprocess.run(
                    [str(PB), 'update', '--from', str(copy)], check=True, timeout=300, capture_output=True))
            finally:
                shutil.rmtree(installed, ignore_errors=True)
            unit = 'paperboat-hostd-u%d.service' % os.getuid()
            survive(endpoint, identifier, 'a host-service restart', lambda: sudo('systemctl', 'restart', unit))
        except BaseException as error:
            primary_error = error
            raise
        finally:
            try:
                if identifier is not None:
                    confirmed('tunnel', 'delete', identifier)
            except BaseException:
                if primary_error is None:
                    raise
                print(json.dumps({'check': 'tunnel-survival', 'cleanup': 'failed'}), flush=True)
            finally:
                server.shutdown()
                server.server_close()
                worker.join(timeout=5)
    assert registration() == before
    print(json.dumps({'check': 'tunnel-survival', 'status': 'PASS', 'pause_resume': True,
                      'daemon_restart': True, 'update': True, 'host_service_restart': True,
                      'enrollment_preserved': True}))


if __name__ == '__main__':
    main()
