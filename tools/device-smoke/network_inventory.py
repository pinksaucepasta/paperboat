#!/usr/bin/env python3
"""Read active-workspace edge and verified native relay inventories."""
import json
import time

from install_lifecycle import registration
from pairing_lifecycle import command_diagnostic, command_result, invoke
import winhost

# Until the daemon holds a verified relay set (fresh pair, restart, resync) relay list answers this documented
# transient ("wait a few seconds and retry"). Triage saw fresh pairs converge within ~7 minutes.
SYNCING = 'relay_inventory_unavailable'
SYNC_SECONDS = 600


def error_code(result):
    if not result.returncode:
        return None
    try:
        return json.loads(result.stdout)['error']['code']
    except (ValueError, KeyError, TypeError):
        return None


def wait_for_relay_sync(binary, seconds=SYNC_SECONDS, sleep=time.sleep, now=time.monotonic):
    """Return once relay list stops answering SYNCING; fail with the last answer if it never does."""
    deadline, delay = now() + seconds, 2
    while True:
        arguments = ('relay', 'list', '--json')
        result = command_result(binary, *arguments, timeout=60)
        if error_code(result) != SYNCING:
            return
        if now() + delay > deadline:
            diagnostic = command_diagnostic([str(binary), *arguments], result.stdout, result.stderr)
            raise RuntimeError('relay inventory still %s after %ds; private diagnostics: %s' % (SYNCING, seconds, diagnostic))
        sleep(delay)
        delay = min(delay * 2, 30)


def main():
    before = registration()
    binary = winhost.pb_path()
    for kind, collection in [('edge', 'regions'), ('relay', 'relays')]:
        if kind == 'relay':
            wait_for_relay_sync(binary)
        receipt = invoke(binary, kind, 'list', '--json', timeout=60)
        assert receipt['schema'] == 'paperboat.' + kind + '-list/v1'
        modes = ('managed-only', 'self-hosted-only') if kind == 'edge' else ('mixed', 'self-hosted-only')
        assert receipt['pool_mode'] in modes
        items = receipt[collection]
        assert isinstance(items, list) and items, 'no network nodes available in the active workspace'
        ids = [(item['source'], item.get('id') or item.get('region'), item['name']) for item in items]
        assert all(all(i) for i in ids) and len(ids) == len(set(ids)), 'network inventory has missing or duplicate regions'
        assert all(item['source'] in ('paperboat', 'self-hosted') and item['status'] for item in items)
        if receipt['pool_mode'] == 'self-hosted-only':
            assert all(item['source'] == 'self-hosted' for item in items)
    assert registration() == before
    print(json.dumps({'check': 'network-inventory', 'status': 'PASS',
                      'edges_and_verified_relays': True, 'enrollment_preserved': True}))


if __name__ == '__main__':
    main()
