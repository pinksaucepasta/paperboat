#!/usr/bin/env python3
"""Port names (`pb port name`, `pb port front-desk`): run on the HOST machine; VIEWER is another machine of the account.

  python3 port_names.py VIEWER [--shadow-machine NAME]

The host serves two synthetic pages, names them from the host, and the viewer (nothing configured there) fetches each
address over HTTPS with normal certificate verification through `pb exec VIEWER -- curl`. Unix hosts and viewers only
(the host starts python3 servers, the viewer needs curl). --shadow-machine NAME (an existing machine of the account) also
checks that a top-level name equal to a machine is refused and that a machine taking an existing top-level name shadows it.
"""
import argparse
import contextlib
import json
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import uuid
from pathlib import Path

import winhost
from install_lifecycle import registration
from pairing_lifecycle import command_result

BINARY = winhost.pb_path()


def pb(*arguments, timeout=120):
    result = command_result(BINARY, *arguments, timeout=timeout)
    try:
        body = json.loads(result.stdout)
    except ValueError:
        body = None
    data = body.get('data', body) if isinstance(body, dict) and 'ok' in body else body
    code = body['error'].get('code') if isinstance(body, dict) and isinstance(body.get('error'), dict) else None
    return result.returncode, data, code, result.stderr


def pb_ok(*arguments):
    status, data, code, stderr = pb(*arguments)
    if status:
        raise RuntimeError('pb %s failed (%s)' % (' '.join(arguments[:4]), code or stderr[-200:]))
    return data


def free_port():
    with socket.socket() as probe:
        probe.bind(('127.0.0.1', 0))
        return probe.getsockname()[1]


@contextlib.contextmanager
def page(root, marker):
    directory = Path(tempfile.mkdtemp(dir=root))
    (directory / 'who').write_text(marker)
    port = free_port()
    server = subprocess.Popen([sys.executable, '-m', 'http.server', str(port), '--bind', '127.0.0.1', '--directory', str(directory)],
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        yield port
    finally:
        server.terminate()
        try:
            server.wait(timeout=10)
        except subprocess.TimeoutExpired:
            server.kill()


def fetch(viewer, address, expected, wait=90):
    """curl on the viewer until the address answers `expected` (the guard publishes new names within seconds)."""
    deadline, last = time.monotonic() + wait, ''
    while True:
        result = command_result(BINARY, 'exec', viewer, '--timeout', '30s', '--', 'curl', '-fsS', '--noproxy', '*',
                                '--max-time', '10', 'https://' + address + '/who', timeout=60)
        last = result.stdout.strip() or result.stderr.strip()[-200:]
        if result.returncode == 0 and result.stdout.strip() == expected:
            return
        if time.monotonic() >= deadline:
            raise RuntimeError('%s answered %r, wanted %r' % (address, last, expected))
        time.sleep(3)


def version(viewer=None):
    words = ['sh', '-c', '"$HOME/.local/bin/pb" --version']
    if viewer:
        result = command_result(BINARY, 'exec', viewer, '--timeout', '30s', '--', *words, timeout=60)
    else:
        result = command_result(BINARY, '--version')
    return result.stdout.strip() if result.returncode == 0 else 'unknown'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('viewer')
    parser.add_argument('--shadow-machine')
    args = parser.parse_args()
    before = registration()
    machines = pb_ok('machine', 'list', '--all', '--json')
    machines = machines.get('machines', machines.get('items', [])) if isinstance(machines, dict) else machines
    host = next(m['alias'] for m in machines if m['id'] == before['machine_id'])
    assert any(m['alias'] == args.viewer for m in machines), 'the viewer is not a machine of this account'
    domain = pb_ok('config', 'local-access', 'show', '--json')['domain']
    prefix = 'pn' + uuid.uuid4().hex[:6]
    steps, created, failure = {}, [], None
    root = tempfile.mkdtemp(prefix='pb-port-names-')

    def step(name, action):
        try:
            action()
            steps[name] = 'PASS'
        except Exception as error:
            steps[name] = 'FAIL: ' + str(error)[:300]
            raise

    def set_name(name, port, *scope):
        pb_ok('port', 'name', 'set', name, str(port), *scope, '--json')
        created.append((name, scope))

    try:
        with page(root, 'portA') as a, page(root, 'portB') as b:
            def names():
                set_name('app', a, '--machine', host)
                set_name(prefix + 'all', a, '--all-machines')
                set_name(prefix + 'all', b, '--machine', host)
                set_name(prefix + 'top', a, '--top-level', '--machine', host)
                pb_ok('port', 'front-desk', host, str(a), 'http')
                status, _, code, _ = pb('port', 'name', 'set', args.viewer, str(a), '--top-level', '--machine', host, '--json')
                if status == 0:
                    created.append((args.viewer, ('--top-level',)))
                assert status != 0 and code == 'name_is_machine', 'a top-level name equal to a machine was not refused (%s)' % code
            step('set_names', names)
            for name, address, expected in [
                    ('machine_name', 'app.%s.%s' % (host, domain), 'portA'),
                    ('machine_beats_all_machines', '%sall.%s.%s' % (prefix, host, domain), 'portB'),
                    ('top_level', '%stop.%s' % (prefix, domain), 'portA'),
                    ('port_number', '%d.%s.%s' % (a, host, domain), 'portA'),
                    ('front_desk', 'zz%s.%s.%s' % (prefix, host, domain), 'portA')]:
                step('viewer_' + name, lambda address=address, expected=expected: fetch(args.viewer, address, expected))

            def listing():
                records = pb_ok('port', 'name', 'list', '--json')
                records = records.get('names', records) if isinstance(records, dict) else records
                top = [r for r in records if r['name'] == prefix + 'top']
                assert len(top) == 1 and top[0]['shadowed'] is False, 'the top-level record is missing or shadowed'
            step('list', listing)
            if args.shadow_machine:
                def shadow():
                    name = args.shadow_machine
                    assert any(m['alias'] == name for m in machines), 'the shadow machine is not in the account'
                    status, _, code, _ = pb('port', 'name', 'set', name, str(a), '--top-level', '--machine', host, '--json')
                    if status == 0:
                        created.append((name, ('--top-level',)))
                    assert status != 0 and code == 'name_is_machine', 'a machine-named top-level name was accepted'
                step('shadow_refused', shadow)
    except Exception as error:
        failure = error
    finally:
        cleanup = []
        for name, scope in reversed(created):
            where = ('--top-level',) if '--top-level' in scope else scope
            status, _, _, _ = pb('port', 'name', 'unset', name, *where)
            if status:
                cleanup.append(name)
        status, _, _, _ = pb('port', 'front-desk', host, '80', 'http', '--json')
        if status:
            cleanup.append('front-desk')
        shutil.rmtree(root, ignore_errors=True)
    steps['cleanup'] = 'PASS' if not cleanup else 'FAIL: ' + ', '.join(cleanup)
    ok = failure is None and not cleanup and registration() == before
    print(json.dumps({'check': 'port-names', 'target': args.viewer, 'status': 'PASS' if ok else 'FAIL', 'steps': steps,
                      'versions': {'host': version(), 'viewer': version(args.viewer)}, 'enrollment_unchanged': registration() == before}))
    sys.exit(0 if ok else 1)


if __name__ == '__main__':
    main()
