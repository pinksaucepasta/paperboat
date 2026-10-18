#!/usr/bin/env python3
"""F15-02/F15-03: bugreport and local diagnostics hold no secrets; the uploaded reference resolves.

Every credential, private key and ENV value on this device is gathered in memory, a
bugreport is uploaded, and neither the bundle nor the diagnostics directory may contain any
of those values. The printed receipt never includes a secret.
"""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import urllib.request
import zipfile

HOME = Path.home()
PB = HOME / '.local/bin/pb'
CONFIG = HOME / '.config/paperboat'
STATE = HOME / '.local/state/paperboat'
MARKER = 'pbsmoke-env-' + os.urandom(8).hex()


def run(*args, stdin=None):
    result = subprocess.run([str(PB), *args], input=stdin, capture_output=True, text=True,
                            timeout=120)
    return result.returncode, result.stdout


def leaves(value, path=''):
    if isinstance(value, dict):
        for key, item in value.items():
            yield from leaves(item, path + '/' + key)
    elif isinstance(value, list):
        for item in value:
            yield from leaves(item, path + '[]')
    elif isinstance(value, str):
        yield path, value


def secrets():
    """Credential material that must never appear: tokens, private keys, ENV marker."""
    found = {MARKER: 'env-value'}
    for path in (CONFIG / 'credentials/secrets').glob('*.secret'):
        label = re.sub(r'[0-9a-f]{16,}', '*', path.name)
        text = path.read_text(errors='replace').strip()
        try:
            for key, value in leaves(json.loads(text)):
                if len(value) >= 24 and 'public' not in path.name:
                    found[value] = label + key
        except ValueError:
            found[text] = label
    for name in ('machine-control.json', 'runtime-identity.json'):
        try:
            for key, value in leaves(json.loads((STATE / 'runtime' / name).read_text())):
                if key == '/credential':
                    found[value] = name + key
        except (OSError, ValueError):
            pass
    return {value: label for value, label in found.items() if len(value) >= 16}


def leaked(blob, values):
    return sorted({label for value, label in values.items() if value.encode() in blob})


def access_token():
    for path in (CONFIG / 'credentials/secrets').glob('profile-*-access.secret'):
        return path.read_text().strip()
    raise RuntimeError('no CLI access credential to resolve the support reference')


def lookup_reference(reference):
    server = json.loads((CONFIG / 'config.json').read_text())['server_url']
    request = urllib.request.Request(server + '/v1/support/' + reference,
                                     headers={'Authorization': 'Bearer ' + access_token()})
    with urllib.request.urlopen(request, timeout=30) as response:
        lookup = json.loads(response.read())['data']
    if lookup.get('support_reference') != reference or lookup.get('status') != 'recorded':
        raise RuntimeError('support lookup did not resolve the uploaded reference; status=' +
                           str(lookup.get('status')))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--no-lookup', action='store_true', help='F15-03 only: skip the server support lookup')
    args = parser.parse_args()
    code, _ = run('env', 'set', 'PB_SMOKE_BUGREPORT', '--value-stdin', '--json', stdin=MARKER)
    set_env = code == 0
    try:
        # Terminal-like output must not reach diagnostics either.
        output_marker = 'pbsmoke-output-' + os.urandom(8).hex()
        if output_marker not in run('exec', args.target, '--', 'sh', '-c', 'echo ' + output_marker)[1]:
            raise RuntimeError('exec output marker was not returned')
        code, out = run('bugreport', '--upload', '--json')
        report = json.loads(out)
        if code or not report.get('bundle_created') or report.get('uploaded') is not True:
            raise RuntimeError('bugreport did not create and upload a bundle')
        reference = report['correlation_id']
        if report.get('server_correlation_id') != reference or not re.fullmatch(r'support_[0-9a-f-]{36}', reference):
            raise RuntimeError('uploaded report reference differs from the local one')
        values = secrets()
        values[output_marker] = 'exec-output'
        bundle = Path(report['bundle_path'])
        with zipfile.ZipFile(bundle) as archive:
            blob = b''.join(archive.read(name) for name in archive.namelist())
        if oct(bundle.stat().st_mode & 0o077) != '0o0':
            raise RuntimeError('bundle is readable by other users')
        if leaked(blob, values):
            raise RuntimeError('bugreport bundle contains secret material from: ' + ', '.join(leaked(blob, values)))
        diagnostics = b''.join(p.read_bytes() for p in (STATE / 'diagnostics').glob('*') if p.is_file())
        if not diagnostics:
            raise RuntimeError('no local diagnostics were written')
        if leaked(diagnostics, values):
            raise RuntimeError('local diagnostics contain secret material from: ' + ', '.join(leaked(diagnostics, values)))
        if not args.no_lookup:
            lookup_reference(reference)
    finally:
        if set_env:
            challenge = json.loads(run('env', 'unset', 'PB_SMOKE_BUGREPORT', '--json')[1])
            token = challenge.get('data', {}).get('confirmation_token')
            if token:
                run('env', 'unset', 'PB_SMOKE_BUGREPORT', '--json', '--confirm', token)
    print(json.dumps({'check': 'bugreport-lifecycle', 'target': args.target, 'status': 'PASS',
                      'bundle_secret_free': True, 'diagnostics_secret_free': True,
                      'secrets_checked': len(values), 'reference_resolves': not args.no_lookup}), flush=True)


if __name__ == '__main__':
    main()
