#!/usr/bin/env python3
"""The machine side of the console-config-sync row (console_pages.py, F11-05): runs ON a throwaway fixture machine and
leaves its state in place for the browser to compare (the machine is removed with the row).
  python3 console_config_fixture.py ALIAS synced     a local repository, connected, assigned two-way, in sync
  python3 console_config_fixture.py ALIAS review     automatic updates off, a new revision waits for approval
  python3 console_config_fixture.py ALIAS approve    the waiting revision is approved: in sync again
  python3 console_config_fixture.py ALIAS conflict   both sides changed the same line: one conflict
  python3 console_config_fixture.py ALIAS resolve    the conflict is resolved keeping the repository: in sync again
Prints one JSON line {"status": "PASS"|"FAIL", "detail": ...}."""
import json
import sys

import config_sync_lifecycle as c

NAME = 'console-config'


def repository():
    listed = c.pb_json('config', 'repository', 'list')['repositories']
    return next(r['id'] for r in listed if r['external_ref'] == str(c.ROOT / 'console.git'))


def state(alias, wanted, error_code=None):
    def there():
        status = c.my_status(alias)
        return status if status['state'] == wanted and (error_code is None or status.get('error_code') == error_code) else None
    try:
        return c.wait('config sync state %s %s' % (wanted, error_code or ''), there)
    except AssertionError as error:
        status = c.my_status(alias)
        raise AssertionError('%s; last state=%s error_code=%s' % (error, status.get('state'), status.get('error_code')))


def conflicts(alias):
    return [x for x in c.pb_json('config', 'conflict', 'list')['conflicts'] if x['environment_name'] == alias]


def run(alias, action):
    c.use_platform_paths()
    managed, bare = c.ROOT / 'managed.txt', c.ROOT / 'console.git'
    if action == 'synced':
        c.ROOT.mkdir(parents=True, exist_ok=True)
        c.new_bare('console', 'seed\n')
        made = c.pb_json('config', 'repository', 'connect', str(bare), '--name', NAME)['repository']['id']
        c.write_machine_file(made, managed)
        c.pb_json('config', 'sync', 'validate')
        c.apply_configuration()
        c.wait('the seed on the machine', lambda: c.read(managed) == 'seed\n')
        state(alias, 'healthy')
    elif action == 'review':
        c.write_machine_file(repository(), managed, automatic='false')
        c.apply_configuration()
        c.edit_repository(bare, 'needs approval\n')
        state(alias, c.my_status(alias)['state'])
        c.wait('review_required', lambda: c.my_status(alias).get('error_code') == 'review_required')
    elif action == 'approve':
        c.pb_json('config', 'approve', alias)
        c.wait('the approved revision on the machine', lambda: c.read(managed) == 'needs approval\n')
        c.write_machine_file(repository(), managed)
        c.apply_configuration()
        state(alias, 'healthy')
    elif action == 'conflict':
        c.systemctl('stop', c.UNIT)
        managed.write_text('machine side\n')
        c.edit_repository(bare, 'repository side\n')
        c.systemctl('start', c.UNIT)
        c.wait('one listed conflict', lambda: len(conflicts(alias)) == 1)
    elif action == 'resolve':
        path = conflicts(alias)[0]['conflict']['path']
        c.pb_json('config', 'conflict', 'resolve', alias, path, '--keep', 'repository')
        c.wait('no conflict left', lambda: not conflicts(alias))
        c.wait('the repository side on the machine', lambda: c.read(managed) == 'repository side\n')
        state(alias, 'healthy')
    else:
        raise SystemExit(__doc__)


if __name__ == '__main__':
    try:
        run(sys.argv[1], sys.argv[2])
    except Exception as error:  # noqa: BLE001 the row reports the reason
        print(json.dumps({'status': 'FAIL', 'detail': '%s: %s' % (type(error).__name__, str(error)[:300])}))
        sys.exit(1)
    print(json.dumps({'status': 'PASS', 'detail': sys.argv[2]}))
