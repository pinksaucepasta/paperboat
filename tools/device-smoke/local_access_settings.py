#!/usr/bin/env python3
"""Exercise installed local-access settings through the ordinary apply owner (domain set/reset)."""
import json
import sys
import urllib.request

import winhost
from install_lifecycle import registration
from pairing_lifecycle import invoke, command_result, command_diagnostic, runtime_root


def main():
    password = sys.stdin.readline().rstrip('\n') or None
    binary = winhost.pb_path()
    before = registration()
    original = {'domain': invoke(binary, 'config', 'local-access', 'show', '--json')['domain']}
    configuration = json.loads((runtime_root() / 'machine-registration.json').read_text())
    with urllib.request.urlopen(configuration['server_url'].rstrip('/') + '/v1/client-configuration', timeout=20) as response:
        metadata = json.load(response)
    discovered = metadata.get('data', metadata)['local_access_base_domain'].strip().lower()
    assert discovered

    def change(*arguments):
        argv = ['config', 'local-access', *arguments, '--json']
        result = command_result(binary, *argv, terminal=True, password=password, timeout=180)
        if result.returncode:
            path = command_diagnostic([str(binary), *argv], result.stdout, result.stderr)
            raise RuntimeError('local settings failed; private diagnostics: ' + str(path))

    def current_domain():
        return invoke(binary, 'config', 'local-access', 'show', '--json')['domain']

    try:
        change('domain', 'reset')
        assert current_domain() == discovered
        change('domain', 'set', discovered)
        assert current_domain() == discovered
    finally:
        primary = sys.exc_info()[1]
        cleanup_errors = []
        for arguments in [('domain', 'set', original['domain'])]:
            try:
                change(*arguments)
            except Exception as error:
                cleanup_errors.append(error)
        if cleanup_errors:
            if primary is None:
                raise cleanup_errors[0]
            print('Local settings cleanup also failed; inspect the protected command receipts.', file=sys.stderr)
    assert current_domain() == original['domain']
    assert registration() == before
    print(json.dumps({'check': 'local-access-settings', 'status': 'PASS',
                      'domain_set_reset': True, 'enrollment_preserved': True}))


if __name__ == '__main__':
    main()
