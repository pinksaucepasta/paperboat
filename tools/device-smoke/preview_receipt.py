"""Read only the exact resource created by the smoke preview command."""
import json
import re
import sys
from datetime import datetime
from urllib.parse import urlsplit


def receipt_timestamp(value):
    # Python 3.9 accepts only three or six fractional digits. Go's RFC3339Nano
    # receipts use zero to nine; normalize only the fraction to datetime's
    # microsecond precision and retain the original numeric timezone offset.
    match = re.fullmatch(
        r'([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})'
        r'(?:\.([0-9]{1,9}))?(Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])', value)
    if not match:
        raise ValueError('invalid RFC3339 timestamp')
    fraction = '.' + (match[2] + '000000')[:6] if match[2] else ''
    return datetime.fromisoformat(match[1] + fraction + match[3].replace('Z', '+00:00'))


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
