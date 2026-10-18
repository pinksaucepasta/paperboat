#!/usr/bin/env python3
"""Exercise durable tunnel and route management on the enrolled device."""
import functools
import http.server
import json
from pathlib import Path
import tempfile
import threading
import uuid

from install_lifecycle import registration
from pairing_lifecycle import confirmed_command, invoke
from preview_lifecycle import QuietHandler


def main():
    binary = Path.home() / '.local/bin/pb'
    before = registration()
    name = 'smoke-tunnel-' + uuid.uuid4().hex[:16]
    renamed = name + '-new'
    identifier = None
    primary_error = None
    with tempfile.TemporaryDirectory(prefix='paperboat-tunnel-') as directory:
        (Path(directory) / 'index.html').write_text('synthetic tunnel origin')
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0),
                    functools.partial(QuietHandler, directory=directory))
        worker = threading.Thread(target=server.serve_forever, daemon=True)
        worker.start()

        def call(*args):
            return invoke(binary, 'tunnel', *args, '--json', timeout=90)

        def owned():
            # A deleted tunnel stays listed as a tombstone (desired_state=deleted).
            return [item for item in call('list', '--q', name)['items']
                    if item['name'] in (name, renamed) and item['account_id'] == before['account_id']
                    and item['desired_state'] != 'deleted']

        try:
            created = call('create', name, '--port', str(server.server_port),
                           '--duration', '5m', '--wait', '--timeout', '60s')
            identifier = created['tunnel']['id']
            assert created['tunnel']['account_id'] == before['account_id']
            assert created['connector']['state'] == 'ready' and not created.get('pending', False)
            shown = call('show', identifier)
            assert shown['id'] == identifier and shown['name'] == name
            assert any(item['id'] == identifier for item in owned())
            routes = call('route', 'list', identifier)['items']
            assert len(routes) == 1 and routes[0]['origin']['address'].endswith(':' + str(server.server_port))
            added = call('route', 'add', identifier, '--name', 'smoke-extra',
                         '--path', '/smoke-extra', '--priority', '10',
                         '--to', 'http://127.0.0.1:' + str(server.server_port))['route']
            updated = call('route', 'update', identifier, added['id'],
                           '--name', 'smoke-edited', '--path', '/smoke-edited')['route']
            assert updated['id'] == added['id'], 'route update changed its resource identity'
            assert updated['name'] == 'smoke-edited', 'route update did not publish the requested name'
            assert updated['path_prefix'] == '/smoke-edited'
            confirmed_command(binary, 'tunnel', 'route', 'remove', identifier, added['id'])
            assert not any(r['id'] == added['id'] for r in call('route', 'list', identifier)['items'])
            mutation = call('rename', identifier, renamed)
            assert mutation['tunnel']['id'] == identifier and mutation['tunnel']['name'] == renamed
            assert call('show', renamed)['id'] == identifier
            confirmed_command(binary, 'tunnel', 'delete', identifier)
            assert not owned()
            identifier = None
        except BaseException as error:
            primary_error = error
            raise
        finally:
            try:
                # A create can fail after the server mutation. Recover only the
                # exact random name/account owned by this fixture.
                for item in owned():
                    assert identifier is None or item['id'] == identifier
                    confirmed_command(binary, 'tunnel', 'delete', item['id'])
            except BaseException:
                if primary_error is None:
                    raise
                print(json.dumps({'check': 'tunnel-manage', 'cleanup': 'failed'}), flush=True)
            finally:
                server.shutdown()
                server.server_close()
                worker.join(timeout=5)
    assert registration() == before
    print(json.dumps({'check': 'tunnel-manage', 'status': 'PASS',
                      'create_show_list_delete': True, 'route_add_update_remove': True,
                      'rename': True, 'enrollment_preserved': True}))


if __name__ == '__main__':
    main()
