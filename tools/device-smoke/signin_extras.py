#!/usr/bin/env python3
"""API-token sign-in, `pb recordings key status` and `pb config sync schema`, on a signed-in paired machine.

  signin_extras.py [--team SLUG]

Makes one API token (one day, revoked by id on every exit path) and signs in with it from throwaway HOME
directories only, so the machine's own sign-in and daemon are never touched: `pb login --api-token -` (stdin),
`pb login --api-token TOKEN` and `pb login` with PAPERBOAT_API_TOKEN set. After the revoke the signed-in token
profile is refused and so is a new sign-in with the token.
No output ever contains the token.
"""
import argparse
import json
import os
import re
import subprocess
import sys
import tempfile
import uuid
from pathlib import Path

PB = str(Path.home() / '.local/bin/pb')
SECRET = []


def run(*args, home=None, stdin='', env=None, timeout=90):
    values = dict(os.environ, **(env or {}))
    if home:
        for key in ('XDG_CONFIG_HOME', 'XDG_STATE_HOME', 'XDG_DATA_HOME', 'XDG_CACHE_HOME'):
            values.pop(key, None)
        values['HOME'] = home
    done = subprocess.run([PB, *args], input=stdin, capture_output=True, text=True, timeout=timeout, env=values)
    return done.returncode, done.stdout, done.stderr


def clean(text):
    for value in SECRET:
        text = text.replace(value, '<token>')
    return ' '.join(text.split())[-500:]


def ok(*args, **options):
    code, out, err = run(*args, **options)
    if code:
        raise AssertionError('pb %s: exit %d: %s' % (' '.join(a for a in args if a not in SECRET), code, clean(out + err)))
    return out


def data(text):
    value = json.loads(text[text.index('{'):])
    return value.get('data', value) if 'ok' in value else value


def own_identity():
    status = data(ok('auth', 'status', '--json'))
    return {k: v for k, v in status.items() if k in ('signed_in', 'email', 'account_id', 'workspace')}


def schema():
    with tempfile.TemporaryDirectory(prefix='pb-signin-extras-') as home:
        document = json.loads(ok('config', 'sync', 'schema', home=home))  # offline command: no sign-in in this HOME
    assert '$schema' in document and document.get('type') == 'object' and document.get('properties'), \
        'config sync schema is not a JSON Schema object with properties'
    return sorted(document['properties'])


def key_status(team):
    first = data(ok('recordings', 'key', 'status', '--team', team, '--json'))
    second = data(ok('recordings', 'key', 'status', '--team', team, '--json'))
    generation = next((v for k, v in first.items() if 'generation' in k and isinstance(v, int)), None)
    assert generation is not None and generation >= 0, 'no generation in key status: %s' % sorted(first)
    assert first == second, 'reading the key status changed it'
    text = ok('recordings', 'key', 'status', '--team', team)
    assert text.strip(), 'key status prints nothing without --json'
    return generation


def token_sign_in():
    before = own_identity()
    name = 'covf-signin-' + uuid.uuid4().hex[:8]
    created = ok('api-token', 'create', name, '--expires', '1d', '--json')
    found = re.search(r'"(?:secret|token)"\s*:\s*"([^"]{20,})"', created)
    assert found, 'api-token create --json carries no secret'
    token = found[1]
    SECRET.append(token)
    cells = []
    try:
        for label, arguments, stdin in (('stdin', ('login', '--api-token', '-'), token + '\n'),
                                        ('argument', ('login', '--api-token', token), '')):
            with tempfile.TemporaryDirectory(prefix='pb-signin-extras-') as home:
                code, out, err = run(*arguments, '--json', home=home, stdin=stdin)
                assert code == 0, 'pb login --api-token (%s): exit %d: %s' % (label, code, clean(out + err))
                assert token not in out + err, 'pb login (%s) printed the token' % label
                status = data(ok('auth', 'status', '--json', home=home))
                assert status.get('signed_in') is True, 'auth status after %s token sign-in is not signed in' % label
                assert data(ok('machine', 'list', '--json', home=home)).get('machines'), 'token profile lists no machines'
                leaked = [p for p in Path(home).rglob('*') if p.is_file() and p.stat().st_mode & 0o077
                          and token.encode() in p.read_bytes()]
                assert not leaked, 'the token is in a file others can read'
                cells.append(label)
        with tempfile.TemporaryDirectory(prefix='pb-signin-extras-') as kept:
            ok('login', '--json', home=kept, env={'PAPERBOAT_API_TOKEN': token})  # `pb login` reads the variable
            assert data(ok('machine', 'list', '--json', home=kept)).get('machines'), 'PAPERBOAT_API_TOKEN profile lists no machines'
            cells.append('environment')
            ok('api-token', 'revoke', name, '--yes')
            name = None
            code, out, err = run('machine', 'list', '--json', home=kept)
            assert code != 0 and re.search(r'unauthenticated|session is no longer valid|revoked', out + err, re.I), \
                'a profile signed in with a revoked token still works: exit %d %s' % (code, clean(out + err))
            cells.append('revoked-profile-refused')
        with tempfile.TemporaryDirectory(prefix='pb-signin-extras-') as home:
            code, out, err = run('login', '--api-token', '-', '--json', home=home, stdin=token + '\n')
            assert code != 0 and token not in out + err, 'signing in with a revoked token: exit %d %s' % (code, clean(out + err))
            cells.append('revoked-login-refused')
    finally:
        if name:
            run('api-token', 'revoke', name, '--yes')
    assert own_identity() == before, "the machine's own sign-in changed"
    assert data(ok('doctor', '--json'))['overall'] == 'healthy', 'doctor is not healthy after the token sign-ins'
    return cells


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--team', default=os.environ.get('PAPERBOAT_SMOKE_TEAM') or 'paperboat-smoke-paid')
    args = parser.parse_args()
    failed = False
    for check, action in (('config-sync-schema', schema), ('recordings-key-status', lambda: key_status(args.team)),
                          ('api-token-signin', token_sign_in)):
        try:
            print(json.dumps({'check': check, 'status': 'PASS', 'detail': action()}), flush=True)
        except Exception as error:  # every check runs; each failure is reported
            failed = True
            print(json.dumps({'check': check, 'status': 'FAIL', 'detail': clean(str(error))}), flush=True)
    return 1 if failed else 0


if __name__ == '__main__':
    sys.exit(main())
