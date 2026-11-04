"""The signed-in CLI's access token and server URL, read in memory (never printed), on Linux and macOS.

Linux keeps the token in ~/.config/paperboat/credentials/secrets/profile-*-access.secret and the server in
config.json. macOS keeps the token in the login keychain (go-keyring, service paperboat, account = the
profile's access_secret_ref) and may have no config.json until a setting changes, so the server comes from
`pb config show --json` there. The login keychain must be unlocked.
"""
import base64
import glob
import json
import os
from pathlib import Path
import subprocess

HOME = Path.home()
DARWIN = os.uname().sysname == 'Darwin'
CONFIG = HOME / ('Library/Application Support/paperboat' if DARWIN else '.config/paperboat')
PB = HOME / '.local/bin/pb'


def access_token():
    secrets = glob.glob(str(CONFIG / 'credentials/secrets/profile-*-access.secret'))
    if secrets:
        return Path(secrets[0]).read_text().strip()
    if not DARWIN:
        raise RuntimeError('no CLI access secret file; sign in with pb login first')
    ref = json.loads(Path(glob.glob(str(CONFIG / 'credentials/profiles/*.json'))[0]).read_text())['access_secret_ref']
    out = subprocess.run(['/usr/bin/security', 'find-generic-password', '-s', 'paperboat', '-wa', ref],
                         capture_output=True, text=True, timeout=30)
    if out.returncode:
        raise RuntimeError('cannot read the CLI access token from the keychain; unlock the login keychain')
    value = out.stdout.strip()
    if value.startswith('go-keyring-base64:'):
        return base64.b64decode(value[len('go-keyring-base64:'):]).decode()
    if value.startswith('go-keyring-encoded:'):
        return bytes.fromhex(value[len('go-keyring-encoded:'):]).decode()
    return value


def server_url():
    config = CONFIG / 'config.json'
    if config.exists():
        return json.loads(config.read_text())['server_url']
    shown = json.loads(subprocess.run([str(PB), 'config', 'show', '--json'], capture_output=True, text=True, timeout=30).stdout)
    return shown.get('data', shown)['server_url']
