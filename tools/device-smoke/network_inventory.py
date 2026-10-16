#!/usr/bin/env python3
"""Read active-workspace edge and verified native relay inventories."""
import json
from pathlib import Path

from install_lifecycle import registration
from pairing_lifecycle import invoke


def main():
    before = registration()
    binary = Path.home() / '.local/bin/pb'
    for kind, collection, identity in [('edge', 'edges', 'id'), ('relay', 'relays', 'relay_id')]:
        receipt = invoke(binary, kind, 'list', '--json', timeout=60)
        assert receipt['schema'] == 'paperboat.' + kind + '-list/v1'
        assert receipt['pool_mode'] in ('managed-only', 'self-hosted-only')
        items = receipt[collection]
        assert isinstance(items, list) and items, 'no network nodes available in the active workspace'
        ids = [item[identity] for item in items]
        assert all(ids) and len(ids) == len(set(ids)), 'network inventory has missing or duplicate identities'
        assert all(item['source'] in ('paperboat', 'self-hosted') and item['status'] for item in items)
        if receipt['pool_mode'] == 'self-hosted-only':
            assert all(item['source'] == 'self-hosted' for item in items)
    assert registration() == before
    print(json.dumps({'check': 'network-inventory', 'status': 'PASS',
                      'edges_and_verified_relays': True, 'enrollment_preserved': True}))


if __name__ == '__main__':
    main()
