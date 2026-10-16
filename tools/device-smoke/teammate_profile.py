"""Separate controlled teammate credentials from the enrolled device's identity."""
import json
import os
from pathlib import Path


def profile_root():
    if os.uname().sysname == 'Darwin':
        # Darwin's local API socket has a104-byte pathname limit.
        return Path.home() / '.pb-smoke'
    return Path.home() / '.local/state/paperboat/device-smoke/teammate'


def environment(root):
    env = dict(os.environ)
    # macOS security uses HOME to locate the existing login Keychain. XDG
    # overrides isolate credential publication without hiding that Keychain.
    if os.uname().sysname != 'Darwin':
        env['HOME'] = str(root / 'home')
    for name, child in [('XDG_CONFIG_HOME', 'config'), ('XDG_DATA_HOME', 'data'),
                        ('XDG_STATE_HOME', 'state'), ('XDG_CACHE_HOME', 'cache')]:
        env[name] = str(root / child)
    return env


def prepare(root, config):
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    (root / 'home').mkdir(exist_ok=True, mode=0o700)
    path = root / 'config.json'
    value = {'server_url': config['server_url'], 'auth': {
        'allow_file_fallback': config.get('auth', {}).get('allow_file_fallback', False),
        'profile_dir': str(root / 'credentials')}}
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(descriptor, 'w') as output:
        json.dump(value, output)
    return path
