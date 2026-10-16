#!/usr/bin/env python3
"""Exercise installed local-access settings through the ordinary apply owner."""
import json
from pathlib import Path
import sys
import uuid
import urllib.request

from install_lifecycle import registration
from pairing_lifecycle import invoke, command_result, command_diagnostic, runtime_root


def main():
    password = sys.stdin.readline().rstrip('\n') or None
    binary = Path.home() / '.local/bin/pb'
    before = registration()
    shown = invoke(binary, 'config', 'local-access', 'show', '--json')
    original = shown['local_access']
    configuration = json.loads((runtime_root() / 'machine-registration.json').read_text())
    with urllib.request.urlopen(configuration['server_url'].rstrip('/') + '/v1/client-configuration', timeout=20) as response:
        metadata = json.load(response)
    discovered = metadata.get('data', metadata)['local_access_base_domain'].strip().lower()
    assert discovered
    name = 'smoke' + uuid.uuid4().hex[:12]

    def change(*arguments):
        argv = ['config', 'local-access', *arguments, '--json']
        result = command_result(binary, *argv, terminal=True, password=password, timeout=180)
        if result.returncode:
            path = command_diagnostic([str(binary), *argv], result.stdout, result.stderr)
            raise RuntimeError('local settings failed; private diagnostics: ' + str(path))
        receipt = json.loads(result.stdout[result.stdout.index('{'):])
        assert receipt.get('ok') is not False
        receipt = receipt.get('data', receipt)
        assert receipt['applied'] is True
        effective = invoke(binary, 'config', 'local-access', 'show', '--json')['local_access']
        assert receipt['local_access'] == effective
        return effective

    try:
        configured = change('alias', 'set', name, 'app', '49151')
        assert any(item['machine_alias'] == name and item['name'] == 'app' and item['port'] == 49151
                   for item in configured['service_aliases'])
        configured = change('proxy', 'set', name, '49152')
        assert any(item['machine_alias'] == name and item['port'] == 49152
                   for item in configured['machine_proxies'])
        configured = change('apply')
        assert configured['domain'] == original['domain']
        configured = change('domain', 'reset')
        assert configured['domain'] == discovered
        configured = change('domain', 'set', configured['domain'])
        assert configured['domain'] == discovered
    finally:
        primary = sys.exc_info()[1]
        cleanup_errors = []
        for arguments in [('domain', 'set', original['domain']),
                          ('alias', 'unset', name, 'app'), ('proxy', 'unset', name)]:
            try:
                change(*arguments)
            except Exception as error:
                cleanup_errors.append(error)
        if cleanup_errors:
            if primary is None:
                raise cleanup_errors[0]
            print('Local settings cleanup also failed; inspect the protected command receipts.', file=sys.stderr)
    final = invoke(binary, 'config', 'local-access', 'show', '--json')['local_access']
    assert final['domain'] == original['domain']
    assert final.get('service_aliases') == original.get('service_aliases')
    assert final.get('machine_proxies') == original.get('machine_proxies')
    assert registration() == before
    print(json.dumps({'check': 'local-access-settings', 'status': 'PASS',
                      'alias_proxy_apply_reset': True, 'enrollment_preserved': True}))


if __name__ == '__main__':
    main()
