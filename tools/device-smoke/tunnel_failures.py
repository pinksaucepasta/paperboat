#!/usr/bin/env python3
"""F8-07: `pb tunnel status/logs/doctor` explain real failures.

Induces two real failures on a working durable tunnel and checks that each is named, with what to
do, in all three commands, and that the tunnel returns to ready by itself once the cause is fixed:
  1. the local app stops           -> origin_unreachable, log tunnel_origin_unreachable
  2. the Paperboat host service stops (Linux, Windows) -> connector_offline
"""
import json
from pathlib import Path
import platform
import tempfile
import time
import uuid

from install_lifecycle import registration
from pairing_lifecycle import command_result
from preview_lifecycle import stoppable_origin
from tunnel_survival import PB, confirmed, host_service, run, wait_status
import winhost

CAUSE_SECONDS = 150
RECOVER_SECONDS = 150


def human(*args):
    result = command_result(PB, *args, timeout=120)
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


def main():
    before = registration()
    name = 'smoke-tunnel-explain-' + uuid.uuid4().hex[:10]
    identifier = None
    stopped_host = None
    primary_error = None
    result = {'check': 'tunnel-failures', 'status': 'PASS'}
    with tempfile.TemporaryDirectory(prefix='paperboat-tunnel-explain-') as directory:
        (Path(directory) / 'index.html').write_text('synthetic tunnel origin')
        port, stop_origin = stoppable_origin(directory, 'synthetic tunnel origin')  # on the device itself for Windows
        try:
            created = run('tunnel', 'create', name, '--port', str(port), '--duration', '30m', '--wait', '--timeout', '90s')
            identifier = created['tunnel']['id']
            endpoint = created['tunnel']['stable_endpoint']
            wait_status(endpoint, 200, 'after creation')
            wait_code(identifier, 'ready', 60, 'creation')

            stop_origin()
            wait_code(identifier, 'origin_unreachable', CAUSE_SECONDS, 'stopping the local app')
            logs = explained(identifier, 'origin_unreachable')
            assert 'tunnel_origin_unreachable' in logs, 'tunnel logs do not record the local app going down'
            origin = run('tunnel', 'status', identifier)['dimensions']['origin']['code']
            assert origin.startswith('origin_'), 'origin dimension does not carry the reason: ' + origin
            again, stop_origin = stoppable_origin(directory, 'synthetic tunnel origin', port)
            assert again == port, 'the origin did not come back on its port'
            wait_code(identifier, 'ready', RECOVER_SECONDS, 'restarting the local app')
            wait_status(endpoint, 200, 'after the local app returned')
            assert 'tunnel_origin_recovered' in human('tunnel', 'logs', identifier), 'tunnel logs do not record the recovery'
            result['origin_stopped'] = origin

            if winhost.device() or platform.system() == 'Linux':
                stopped_host = host_service('stop')
                wait_code(identifier, 'connector_offline', CAUSE_SECONDS, 'stopping the host service')
                explained(identifier, 'connector_offline')
                host_service('start', stopped_host)
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
                    host_service('start', stopped_host)
                if identifier is not None:
                    confirmed('tunnel', 'delete', identifier)
            except BaseException:
                if primary_error is None:
                    raise
                print(json.dumps({'check': 'tunnel-failures', 'cleanup': 'failed'}), flush=True)
            finally:
                try:
                    stop_origin()
                except Exception:
                    pass
    assert registration() == before
    result['enrollment_preserved'] = True
    print(json.dumps(result))


if __name__ == '__main__':
    main()
