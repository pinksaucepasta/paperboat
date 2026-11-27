#!/usr/bin/env python3
"""Container side: sign the controlled teammate profile in with an API token read from stdin (never saved).

An already signed-in profile prints 'signed-in' and reads nothing. Exits 0 only when the profile is signed in.
A signed-in profile counts only when the server answers it (`team list`): `auth status` reads the local profile, and
called a session that had expired days before "already signed in". With TEAM the teammate must also be an active member
of it (`team list` names only those), else exit 3.

Usage: teammate_login.py [TEAM]   (HOME of the enrolled device user; the profile lives under profile_root())
"""
import json
import os
import subprocess
import sys
from pathlib import Path

from teammate_profile import environment, prepare, profile_root

PB = str(Path.home() / '.local/bin/pb')


def main():
    root = profile_root()
    config = root / 'config.json'
    if not config.exists():
        device = ('Library/Application Support/paperboat' if os.uname().sysname == 'Darwin'
                  else '.config/paperboat')
        file = Path.home() / device / 'config.json'
        if file.exists():
            prepare(root, json.loads(file.read_text()))
        else:  # pb writes config.json only once a setting changes: ask pb for the server and fallback it uses
            shown = json.loads(subprocess.run([PB, 'config', 'show', '--json'], capture_output=True, text=True, timeout=30).stdout)
            shown = shown.get('data', shown)
            prepare(root, {'server_url': shown['server_url'], 'auth': {'allow_file_fallback': bool(shown.get('auth_file_fallback'))}})
    env = environment(root)
    for name in ('config', 'data', 'state', 'cache', 'home'):
        (root / name).mkdir(parents=True, exist_ok=True, mode=0o700)
    def pb(*arguments, **kw):
        return subprocess.run([PB, '--config', str(config), *arguments], env=env, **kw)

    def live():
        """The teams the server lists for this session, or None when it does not answer it."""
        done = pb('team', 'list', '--json', capture_output=True, text=True, timeout=60, stdin=subprocess.DEVNULL)
        if done.returncode:
            return None
        teams = json.loads(done.stdout)
        return teams.get('data', teams) if isinstance(teams, dict) else teams

    team = sys.argv[1] if len(sys.argv) > 1 else ''
    teams = live()
    signed_in_now = teams is not None
    if not signed_in_now:
        # a token sign-in replaces an expired session; no logout, which on macOS shares the login Keychain
        done = pb('login', '--api-token', '-', stdin=sys.stdin)
        if done.returncode:
            return done.returncode
        teams = live()
        if teams is None:
            print('the server does not answer the new teammate session', file=sys.stderr)
            return 1
    if team and not any(t['team_id'] == team for t in teams):
        print('the teammate is NOT an ACTIVE member of %s: invite it and accept before the team rows' % team, file=sys.stderr)
        return 3
    if signed_in_now:
        print('signed-in')
    return 0


if __name__ == '__main__':
    sys.exit(main())
