#!/usr/bin/env python3
"""F15-02/F15-03: bugreport and local diagnostics hold no secrets; the uploaded reference resolves.

Every credential, private key and ENV value on this device is gathered in memory, a
bugreport is uploaded, and neither the bundle nor the diagnostics directory may contain any
of those values. The printed receipt never includes a secret.

On the Windows device (PB_SMOKE_WINDOWS_PB, see winhost.py) the CLI tokens are DPAPI-protected files this check cannot
open, so they are not searched for and the support lookup (which needs the access token) is not run; the ENV value,
the exec output, the stored secret files as they are on disk and the machine credentials are.
"""
import argparse
import base64
import io
import json
import os
from pathlib import Path
import re
import subprocess
import urllib.request
import zipfile

import windows_target
import winhost

HOME = Path.home()
PB = winhost.pb_path()
DEVICE = winhost.device()
DARWIN = os.uname().sysname == 'Darwin'
if DEVICE:  # xdg config and the daemon state root are both under %LOCALAPPDATA% on Windows
    LOCAL = winhost.RemotePath(DEVICE, DEVICE.env('LOCALAPPDATA'))
    CONFIG, STATE, RUNTIME = LOCAL / 'paperboat', LOCAL / 'Paperboat' / 'state', DEVICE.runtime_dir()
elif DARWIN:
    SUPPORT = HOME / 'Library/Application Support'
    CONFIG, STATE, RUNTIME = SUPPORT / 'paperboat', SUPPORT / 'Paperboat/state', SUPPORT / 'Paperboat/runtime'
else:
    CONFIG, STATE = HOME / '.config/paperboat', HOME / '.local/state/paperboat'
    RUNTIME = STATE / 'runtime'
MARKER = 'pbsmoke-env-' + os.urandom(8).hex()


def run(*args, stdin=None):
    result = winhost.pb_text(PB, args, input=stdin, timeout=120)
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


def keychain_tokens():
    """macOS keeps the CLI tokens in the login keychain (go-keyring, service paperboat), not in files."""
    tokens = {}
    for path in (CONFIG / 'credentials/profiles').glob('*.json'):
        profile = json.loads(path.read_text())
        for kind in ('access', 'refresh'):
            ref = profile[kind + '_secret_ref']
            if (CONFIG / 'credentials/secrets' / (ref + '.secret')).exists():
                continue  # the file fallback holds it; secrets() reads it
            out = subprocess.run(['/usr/bin/security', 'find-generic-password', '-s', 'paperboat', '-wa', ref],
                                 capture_output=True, text=True, timeout=30)
            if out.returncode:
                raise RuntimeError('cannot read the CLI %s token from the keychain (exit %d); unlock the login keychain'
                                   % (kind, out.returncode))
            value = out.stdout.strip()
            if value.startswith('go-keyring-base64:'):
                value = base64.b64decode(value[len('go-keyring-base64:'):]).decode(errors='replace')
            elif value.startswith('go-keyring-encoded:'):
                value = bytes.fromhex(value[len('go-keyring-encoded:'):]).decode(errors='replace')
            tokens['keychain-%s-%s' % (kind, path.stem[:8])] = value
    return tokens


def secret_files():
    """(file name, text) of every file-stored CLI secret."""
    directory = CONFIG / 'credentials/secrets'
    if DEVICE:
        return [(name, (directory / name).read_bytes().decode(errors='replace')) for name in DEVICE.files(directory, '*.secret')]
    return [(path.name, path.read_text(errors='replace')) for path in directory.glob('*.secret')]


def secrets():
    """Credential material that must never appear: tokens, private keys, ENV marker."""
    found = {MARKER: 'env-value'}
    if DARWIN and not DEVICE:
        found.update({value: label for label, value in keychain_tokens().items()})
    for name, text in secret_files():
        label = re.sub(r'[0-9a-f]{16,}', '*', name)
        text = text.strip()
        try:
            for key, value in leaves(json.loads(text)):
                if len(value) >= 24 and 'public' not in name:
                    found[value] = label + key
        except ValueError:
            found[text] = label
    for name in ('machine-control.json', 'runtime-identity.json'):
        try:
            for key, value in leaves(json.loads((RUNTIME / name).read_text())):
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
    for label, value in keychain_tokens().items() if DARWIN else ():
        if label.startswith('keychain-access'):
            return value
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
        if output_marker not in run('exec', args.target, '--', *windows_target.target_argv(
                PB, args.target, ['sh', '-c', 'echo ' + output_marker], windows_target.print_script(output_marker)))[1]:
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
        bundle = winhost.device_path(report['bundle_path'])
        with zipfile.ZipFile(io.BytesIO(bundle.read_bytes()) if DEVICE else bundle) as archive:
            blob = b''.join(archive.read(name) for name in archive.namelist())
        # On Windows the bundle is private by the ACL its directory under the profile gives it; only Unix has a mode.
        if not DEVICE and oct(bundle.stat().st_mode & 0o077) != '0o0':
            raise RuntimeError('bundle is readable by other users')
        if leaked(blob, values):
            raise RuntimeError('bugreport bundle contains secret material from: ' + ', '.join(leaked(blob, values)))
        if DEVICE:  # searched on the device: the event files are large and stay there
            if not DEVICE.files(STATE / 'diagnostics'):
                raise RuntimeError('no local diagnostics were written')
            found = sorted({label for value, label in values.items() if DEVICE.count_in_files(STATE / 'diagnostics', '*', value)})
            if found:
                raise RuntimeError('local diagnostics contain secret material from: ' + ', '.join(found))
        else:
            diagnostics = b''.join(p.read_bytes() for p in (STATE / 'diagnostics').glob('*') if p.is_file())
            if not diagnostics:
                raise RuntimeError('no local diagnostics were written')
            if leaked(diagnostics, values):
                raise RuntimeError('local diagnostics contain secret material from: ' + ', '.join(leaked(diagnostics, values)))
        if not args.no_lookup and not DEVICE:
            lookup_reference(reference)
    finally:
        if set_env:
            challenge = json.loads(run('env', 'unset', 'PB_SMOKE_BUGREPORT', '--json')[1])
            token = challenge.get('data', {}).get('confirmation_token')
            if token:
                run('env', 'unset', 'PB_SMOKE_BUGREPORT', '--json', '--confirm', token)
    print(json.dumps({'check': 'bugreport-lifecycle', 'target': args.target, 'status': 'PASS',
                      'bundle_secret_free': True, 'diagnostics_secret_free': True,
                      'secrets_checked': len(values), 'reference_resolves': not args.no_lookup and not DEVICE}), flush=True)


if __name__ == '__main__':
    main()
