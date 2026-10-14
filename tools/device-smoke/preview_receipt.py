"""Read only the exact resource created by the smoke preview command."""
import json
import re
import sys
from urllib.parse import urlsplit


def receipt_value(receipt, field, expected_id=None):
    if not isinstance(receipt, dict) or receipt.get('schema') != 'paperboat.preview-tunnel/v1' or receipt.get('kind') != 'preview_lease':
        raise ValueError('missing canonical preview resource')
    identity = receipt.get('id')
    if not isinstance(identity, str) or not re.fullmatch(r'[A-Za-z0-9_-]{3,128}', identity):
        raise ValueError('invalid preview identity')
    if field == 'id':
        return identity
    if field == 'stopped':
        if identity != expected_id or receipt.get('state') != 'stopped':
            raise ValueError('exact preview was not stopped')
        return ''
    if field != 'endpoint' or receipt.get('state') != 'ready':
        raise ValueError('preview is not ready')
    endpoint = receipt.get('endpoint')
    if not isinstance(endpoint, str) or any(c.isspace() or ord(c) < 32 for c in endpoint):
        raise ValueError('invalid preview endpoint')
    url = urlsplit(endpoint)
    if url.scheme != 'https' or not url.hostname or url.username or url.password or url.query or url.fragment:
        raise ValueError('invalid preview endpoint')
    return endpoint


if __name__ == '__main__':
    try:
        print(receipt_value(json.load(sys.stdin), sys.argv[1], sys.argv[2] if len(sys.argv) > 2 else None))
    except (ValueError, TypeError, IndexError):
        sys.exit('Preview receipt did not prove the requested operation')
