#!/usr/bin/env python3
"""Exercise durable tunnel health, capture, pause/resume and policy commands."""
import functools
import http.server
import json
from pathlib import Path
import tempfile
import threading
import time
import urllib.request
import uuid

from install_lifecycle import registration
from pairing_lifecycle import command_result
from preview_lifecycle import QuietHandler

PB = Path.home() / '.local/bin/pb'


def run(*args, expect_ok=True):
    result = command_result(PB, *args, '--json', timeout=150)
    try:
        value = json.loads(result.stdout)
    except ValueError:
        raise RuntimeError('command did not return JSON: ' + ' '.join(args)) from None
    if expect_ok and (result.returncode or (isinstance(value, dict) and value.get('ok') is False)):
        raise RuntimeError('command failed: ' + ' '.join(args))
    return value.get('data', value) if isinstance(value, dict) and 'ok' in value else value


def confirmed(*args):
    preview = run(*args, expect_ok=False)
    token = preview.get('confirmation_token')
    assert token, 'no confirmation preview for ' + ' '.join(args)
    return run(*args, '--confirm', token)


def wait(predicate, what, seconds=90):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            value = predicate()
        except OSError:
            # Public names can resolve to an edge that is down; try the next address.
            value = None
        if value:
            return value
        time.sleep(2)
    raise RuntimeError('timed out waiting for ' + what)


def main():
    before = registration()
    name = 'smoke-tunnel-life-' + uuid.uuid4().hex[:12]
    identifier = None
    policy = None
    primary_error = None
    with tempfile.TemporaryDirectory(prefix='paperboat-tunnel-life-') as directory:
        (Path(directory) / 'index.html').write_text('synthetic tunnel origin')
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0),
                    functools.partial(QuietHandler, directory=directory))
        worker = threading.Thread(target=server.serve_forever, daemon=True)
        worker.start()
        port = str(server.server_port)
        try:
            created = run('tunnel', 'create', name, '--port', port, '--duration', '30m',
                          '--wait', '--timeout', '90s')
            identifier = created['tunnel']['id']
            endpoint = created['tunnel']['stable_endpoint']
            status = run('tunnel', 'status', identifier)
            assert status['overall_code'] == 'ready' and status['resource_id'] == identifier
            doctor = run('tunnel', 'doctor', identifier)
            assert doctor['overall_code'] == 'ready' and doctor['dimensions']['origin']['status'] == 'ready'
            logs = run('tunnel', 'logs', identifier)
            assert logs['items'] and all(i['tunnel_id'] == identifier for i in logs['items'])
            # Captured request, then replay of that exact capture.
            assert run('tunnel', 'inspect', identifier, '--enable', '--raw')['capture'] == 'enabled'
            wait(lambda: urllib.request.urlopen(endpoint + '/', timeout=20).status == 200, 'tunnel to serve')
            records = wait(lambda: run('tunnel', 'inspect', identifier).get('records'), 'a capture')
            assert records[0]['method'] == 'GET' and records[0]['url'].startswith(endpoint.replace('https', 'http'))
            shown = run('tunnel', 'inspect', identifier, '--id', records[0]['id'])
            assert shown
            replayed = run('tunnel', 'replay', identifier, records[0]['id'])
            assert replayed
            assert run('tunnel', 'inspect', identifier, '--purge')
            assert run('tunnel', 'inspect', identifier, '--disable')['capture'] == 'disabled'
            # Pause stops new traffic, resume restores it with the same identity.
            paused = run('tunnel', 'pause', identifier, '--wait')
            assert paused['tunnel']['desired_state'] == 'paused' and paused['tunnel']['id'] == identifier
            assert run('tunnel', 'status', identifier)['overall_code'] == 'tunnel_paused'
            resumed = run('tunnel', 'resume', identifier, '--wait')
            assert resumed['tunnel']['desired_state'] == 'active' and resumed['tunnel']['id'] == identifier
            # On-demand port policy lives on the machine, not on the tunnel.
            policy = run('tunnel', 'policy', 'allow', before['machine_id'], port, '--access', 'private',
                         '--expires', '30m')
            assert policy['machine_id'] == before['machine_id'] and policy['access_mode'] == 'private'
            got = run('tunnel', 'policy', 'get', policy['id'])
            assert got['id'] == policy['id'] and got['target']['address'].endswith(':' + port)
            confirmed_or_plain = run('tunnel', 'policy', 'revoke', policy['id'], '--generation',
                                     str(got['generation']))
            assert confirmed_or_plain
            policy = None
        except BaseException as error:
            primary_error = error
            raise
        finally:
            try:
                if policy is not None:
                    current = run('tunnel', 'policy', 'get', policy['id'])
                    run('tunnel', 'policy', 'revoke', policy['id'], '--generation', str(current['generation']))
                if identifier is not None:
                    confirmed('tunnel', 'delete', identifier)
            except BaseException:
                if primary_error is None:
                    raise
                print(json.dumps({'check': 'tunnel-lifecycle', 'cleanup': 'failed'}), flush=True)
            finally:
                server.shutdown()
                server.server_close()
                worker.join(timeout=5)
    assert registration() == before
    print(json.dumps({'check': 'tunnel-lifecycle', 'status': 'PASS', 'status_doctor_logs': True,
                      'capture_replay': True, 'pause_resume': True,
                      'port_policy': True, 'enrollment_preserved': True}))


if __name__ == '__main__':
    main()
