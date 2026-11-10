#!/usr/bin/env python3
"""F8-03: a durable tunnel keeps serving through pause/resume, a daemon restart, an update and a host-service restart."""
import contextlib
import functools
import http.server
import json
import os
import platform
import re
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
from preview_lifecycle import QuietHandler, exposed
import winhost

PB = winhost.pb_path()


def run(*args, expect_ok=True):
    result = command_result(PB, *args, '--json', timeout=240)
    value = json.loads(result.stdout)
    if expect_ok and (result.returncode or (isinstance(value, dict) and value.get('ok') is False)):
        raise RuntimeError('command failed: ' + ' '.join(args))
    return value.get('data', value) if isinstance(value, dict) and 'ok' in value else value


def confirmed(*args):
    token = run(*args, expect_ok=False).get('confirmation_token')
    # A refusal (unreachable, forbidden, state) is not a preview: say which, so the cause is in the failure.
    assert token, 'no confirmation preview for %s: %s' % (' '.join(args), {k: preview.get(k) for k in ('outcome', 'code', 'message', 'error') if preview.get(k)})
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


def hostd_label():
    """macOS: the loaded host service label com.pinksaucepasta.paperboat.hostd.u<uid>.<generation>, looked up like the Linux unit."""
    printed = subprocess.run(['sudo', '-n', 'launchctl', 'print', 'system'], capture_output=True, text=True, timeout=30)
    labels = sorted(set(re.findall(r'com\.pinksaucepasta\.paperboat\.hostd\.u%d(?:\.[0-9a-f]+)?\b' % os.getuid(), printed.stdout)))
    if printed.returncode or len(labels) != 1:
        raise RuntimeError('expected exactly one loaded host service label, found %r' % labels)
    return labels[0]


def hostd_unit():
    """The running host service unit: paperboat-hostd-u<uid>-<generation>.service since c98ac40fe (an update starts the
    next generation before the old one retires), so its name is looked up, never assumed."""
    listed = subprocess.run(['systemctl', 'list-units', '--plain', '--no-legend', '--type=service', '--state=running',
                             'paperboat-hostd-u%d-*.service' % os.getuid()], capture_output=True, text=True, timeout=30)
    units = [line.split()[0] for line in listed.stdout.splitlines() if line.strip()]
    if listed.returncode or len(units) != 1:
        raise RuntimeError('expected exactly one running host service unit, found %r' % units)
    return units[0]


def host_service(action, name=None):
    """stop, start or restart this user's host service; returns its name, to start again what was stopped.

    Linux: the systemd unit; macOS: launchctl kickstart (restart only); Windows: the PaperboatHostd-<instance> service."""
    device = winhost.device()
    if device:
        if name is None:
            names = [item['Name'] for item in device.services('PaperboatHostd-*') if item['Status'] == 'Running']
            if len(names) != 1:
                raise RuntimeError('expected exactly one running host service, found %r' % names)
            name = names[0]
        device.service(action, name)
    elif platform.system() == 'Darwin':
        assert action == 'restart', action
        name = hostd_label()
        sudo('launchctl', 'kickstart', '-k', 'system/' + name)
    else:
        name = name or hostd_unit()
        sudo('systemctl', action, name)
    return name


@contextlib.contextmanager
def binary_copy():
    """A copy of the installed pb in a private directory of the device under test, for `pb update --from`."""
    device = winhost.device()
    if device:
        with device.temp_dir('pb-survive') as directory:
            device.copy(PB, directory / 'pb.exe')
            yield directory / 'pb.exe'
        return
    installed = Path(tempfile.mkdtemp(prefix='pb-survive-', dir=Path.home()))
    try:
        os.chmod(installed, 0o700)
        shutil.copy2(PB, installed / 'pb')
        os.chmod(installed / 'pb', 0o700)
        yield installed / 'pb'
    finally:
        shutil.rmtree(installed, ignore_errors=True)


def update_from(copy):
    result = command_result(PB, 'update', '--from', str(copy), timeout=300)
    if result.returncode:
        raise RuntimeError('pb update --from failed: ' + (result.stderr or result.stdout)[-300:])


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
    with tempfile.TemporaryDirectory(prefix='paperboat-tunnel-survive-') as directory, contextlib.ExitStack() as stack:
        (Path(directory) / 'index.html').write_text('synthetic tunnel origin')
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0),
                    functools.partial(QuietHandler, directory=directory))
        worker = threading.Thread(target=server.serve_forever, daemon=True)
        worker.start()
        try:
            port = stack.enter_context(exposed(server.server_port))  # Windows: the device's port that reaches this origin
            created = run('tunnel', 'create', name, '--port', str(port), '--duration', '30m',
                          '--wait', '--timeout', '90s')
            identifier = created['tunnel']['id']
            endpoint = created['tunnel']['stable_endpoint']
            wait_status(endpoint, 200, 'after creation')
            run('tunnel', 'pause', identifier, '--wait')
            assert status_code(endpoint) != 200, 'a paused tunnel still served traffic'
            run('tunnel', 'resume', identifier, '--wait')
            wait_status(endpoint, 200, 'after resume')
            survive(endpoint, identifier, 'a daemon restart', lambda: run('service', 'restart'))
            with binary_copy() as copy:
                survive(endpoint, identifier, 'an update', lambda: update_from(copy))
            survive(endpoint, identifier, 'a host-service restart', lambda: host_service('restart'))
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
