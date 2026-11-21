#!/usr/bin/env python3
"""PB-T1: changing a durable tunnel's address serves the marker at the new address, retires the old one, and the console JSON shows it."""
import json
import time
import urllib.request
import uuid

from install_lifecycle import registration
from pairing_lifecycle import confirmed_command, invoke
from preview_lifecycle import serving
import winhost

SETTLE = 150  # seconds: the old address is retired only after every edge serves the new one


def fetch(url):
    """The body served at url, or None when it does not answer 200 (an error page or no edge is not the marker)."""
    try:
        with urllib.request.urlopen(url + '/', timeout=15) as response:
            return response.read().decode('utf-8', 'replace') if response.status == 200 else None
    except OSError:  # URLError and HTTPError are OSError
        return None


def poll(predicate, seconds=SETTLE):
    deadline = time.monotonic() + seconds
    while True:
        if predicate():
            return True
        if time.monotonic() >= deadline:
            return False
        time.sleep(3)


def main():
    binary = winhost.pb_path()
    before = registration()
    name = 'smoke-tunnel-addr-' + uuid.uuid4().hex[:12]
    marker = 'pb-t1-' + uuid.uuid4().hex
    subdomain = 'smoke-' + uuid.uuid4().hex[:12]
    identifier = None
    failures = []
    receipt = {}

    def check(label, ok, detail=''):
        receipt[label] = bool(ok)
        if not ok:
            failures.append(label + (': ' + detail if detail else ''))
        return ok

    with serving('<html>' + marker + '</html>') as port:

        def call(*args):
            return invoke(binary, 'tunnel', *args, '--json', timeout=180)

        def owned():
            # A deleted tunnel stays listed as a tombstone (desired_state=deleted).
            return [item for item in call('list', '--q', name)['items']
                    if item['name'] == name and item['account_id'] == before['account_id']
                    and item['desired_state'] != 'deleted']

        try:
            created = call('create', name, '--port', str(port), '--duration', '30m', '--wait', '--timeout', '90s')
            identifier = created['tunnel']['id']
            old = created['tunnel']['stable_endpoint']
            _, base = old.removeprefix('https://').split('.', 1)
            new = 'https://%s.%s' % (subdomain, base)
            check('old_serves_marker_before', poll(lambda: marker in (fetch(old) or ''), 90), old)
            moved = call('address', identifier, subdomain, '--wait', '--timeout', '120s')
            check('address_command_names_tunnel', moved['tunnel']['id'] == identifier)
            check('new_serves_marker', poll(lambda: marker in (fetch(new) or '')), new)
            # The console reads GET /v1/tunnels; `pb tunnel list` returns that same item.
            listed = [item for item in owned() if item['id'] == identifier]
            check('console_lists_tunnel', len(listed) == 1)
            shown = listed[0] if listed else {}
            check('console_shows_new_address', shown.get('stable_endpoint') == new,
                  'got %r want %r' % (shown.get('stable_endpoint'), new))
            check('console_address_not_moving',
                  (shown.get('address_change') or {}).get('state') not in ('moving', 'failed'),
                  json.dumps(shown.get('address_change')))
            check('old_stops_serving', poll(lambda: marker not in (fetch(old) or '')), old)
        except BaseException as error:
            failures.append('aborted: %s: %s' % (type(error).__name__, error))
        finally:
            try:
                # A create can fail after the server mutation: remove only this fixture's exact random name/account.
                for item in owned():
                    confirmed_command(binary, 'tunnel', 'delete', item['id'])
                check('cleanup_tunnel_deleted', not owned())
            except BaseException as error:
                failures.append('cleanup failed: %s: %s' % (type(error).__name__, error))
    check('enrollment_preserved', registration() == before)
    if failures:
        print(json.dumps({'check': 'tunnel-address', 'status': 'FAIL', 'failures': failures, 'assertions': receipt}))
        raise SystemExit('PB-T1 failed: ' + '; '.join(failures))
    print(json.dumps({'check': 'tunnel-address', 'status': 'PASS', 'old_serves_marker_before': True,
                      'new_serves_marker': True, 'old_stops_serving': True, 'console_shows_new_address': True,
                      'enrollment_preserved': True, 'assertions': receipt}))


if __name__ == '__main__':
    main()
