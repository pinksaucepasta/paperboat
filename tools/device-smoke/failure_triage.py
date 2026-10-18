"""Carry finite network evidence without copying private command diagnostics."""
import json
from pathlib import Path
import sys

MAX_OUTPUT = 1 << 20
NETWORK_SCHEMA = 'paperboat.smoke-network/v1'


def cli_network_code(output):
    if not isinstance(output, str) or len(output) > MAX_OUTPUT:
        return None
    try:
        value = json.loads(output)
    except (ValueError, RecursionError):
        return None
    if not isinstance(value, dict):
        return None
    error = value.get('error')
    if (value.get('schema_version') == '1.0' and value.get('ok') is False
            and isinstance(error, dict) and error.get('code') == 'network_dns'
            and error.get('category') == 'unavailable_retryable' and error.get('retryable') is True):
        return 'network_dns'
    return None


def emit_network_failure(output):
    code = cli_network_code(output)
    if code:
        print(json.dumps({'schema': NETWORK_SCHEMA, 'code': code}), file=sys.stderr, flush=True)


def network_code(output):
    if len(output) > MAX_OUTPUT:
        return None
    for line in output.splitlines():
        try:
            value = json.loads(line)
        except (ValueError, RecursionError):
            continue
        if value == {'schema': NETWORK_SCHEMA, 'code': 'network_dns'}:
            return 'network_dns'
    return cli_network_code(output)


if __name__ == '__main__':
    try:
        with Path(sys.argv[1]).open() as source:
            print(network_code(source.read(MAX_OUTPUT + 1)) or '')
    except (OSError, UnicodeError):
        print('')
