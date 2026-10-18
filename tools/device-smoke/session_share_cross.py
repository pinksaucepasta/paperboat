#!/usr/bin/env python3
"""F3-06 / C050 / C053 across two enrolled machines and two accounts (run from the controller).

Owner machine OWNER_HOST (ssh alias) is signed in as account A and shares a live terminal with team TEAM;
PEER_HOST (ssh alias) is an enrolled machine signed in as account B, a member of TEAM, which joins it.
Usage: session_share_cross.py OWNER_HOST OWNER_TARGET PEER_HOST TEAM
"""
import argparse
import json
import subprocess
import sys
import time
import uuid

sys.path.insert(0, __import__('pathlib').Path(__file__).resolve().parent.as_posix())
from terminal_lifecycle import Terminal  # noqa: E402

PB = '~/.local/bin/pb'


def ssh(host, command, timeout=90):
    result = subprocess.run(['ssh', '-o', 'BatchMode=yes', host, command], capture_output=True, text=True,
                            timeout=timeout, stdin=subprocess.DEVNULL)
    return result.returncode, result.stdout, result.stderr


def pb(host, *arguments, timeout=90):
    return ssh(host, PB + ' ' + ' '.join(arguments), timeout)


def data(host, *arguments):
    code, out, err = pb(host, *arguments, '--json')
    if code:
        raise RuntimeError('%s pb %s failed: %s' % (host, ' '.join(arguments[:2]), (out + err)[-200:]))
    value = json.loads(out)
    return value.get('data', value) if isinstance(value, dict) else value


STAGE = ['start']


def stage(name):
    STAGE[0] = name


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('owner_host', 'owner_target', 'peer_host', 'team'):
        parser.add_argument(name)
    args = parser.parse_args()
    peer_id = data(args.peer_host, 'auth', 'status')['account']['id']
    owner_id = data(args.owner_host, 'auth', 'status')['account']['id']
    if peer_id == owner_id:
        raise RuntimeError('owner and peer must be different accounts')
    name = 'smoke-share-' + uuid.uuid4().hex[:12]
    owner = joined = None
    identifier = None
    shared = False
    failure = None
    try:
        stage('owner terminal start')
        owner = Terminal(['ssh', '-tt', args.owner_host, PB + ' connect %s --name %s --status-bar off' % (args.owner_target, name)])
        owner.expect(b'\x1b[?2004h', timeout=90)
        owner.expect(owner.marker('READY', 'stty -echo; '))
        stage('owner ready')
        old = uuid.uuid4().hex
        owner.send("printf '\\nPB_OLD_%s\\n' '" + old + "'\r")
        owner.expect('PB_OLD_' + old)
        sessions = data(args.owner_host, 'session', 'list', args.owner_target)['sessions']
        identifier = next(s['id'] for s in sessions if s['name'] == name)
        if pb(args.peer_host, '--workspace', args.team, 'session', 'join', identifier, timeout=40)[0] == 0:
            raise RuntimeError('peer joined an unshared session')
        stage('share viewer')
        data(args.owner_host, 'session', 'share', identifier, '--team', args.team, '--member', peer_id, '--role', 'viewer')
        shared = True
        catalog = data(args.peer_host, '--workspace', args.team, 'session', 'shared')
        if not any(item['id'] == identifier for item in catalog):
            raise RuntimeError('peer shared catalog omitted the shared session')
        if not data(args.owner_host, 'session', 'participants', identifier)['can_manage']:
            raise RuntimeError('owner cannot manage participants')
        stage('peer join start')
        joined = Terminal(['ssh', '-tt', args.peer_host, PB + ' --workspace %s session join %s' % (args.team, identifier)])
        joined.expect('PB_OLD_' + old, timeout=90)
        stage('peer joined and saw recent output')
        blocked = uuid.uuid4().hex
        joined.send("printf '\\nPB_BLOCKED_%s\\n' '" + blocked + "'\r")
        try:
            owner.expect('PB_BLOCKED_' + blocked, timeout=8)
            raise RuntimeError('viewer input reached the shared shell')
        except RuntimeError as error:
            if 'viewer input' in str(error):
                raise
        stage('upgrade to interactive')
        data(args.owner_host, 'session', 'share', identifier, '--team', args.team, '--member', peer_id, '--role', 'interactive')
        typed = uuid.uuid4().hex
        deadline = time.monotonic() + 40
        while True:
            joined.send("printf '\\nPB_TYPED_%s\\n' '" + typed + "'\r")
            try:
                owner.expect('PB_TYPED_' + typed, timeout=6)
                break
            except RuntimeError:
                if time.monotonic() > deadline:
                    raise RuntimeError('interactive input never reached the shared shell')
        joined.expect('PB_TYPED_' + typed, timeout=20)
        stage('remove peer')
        data(args.owner_host, 'session', 'remove', identifier, peer_id)
        joined.wait_exit(30)
        if pb(args.peer_host, '--workspace', args.team, 'session', 'join', identifier, timeout=40)[0] == 0:
            raise RuntimeError('removed peer could join again')
        owner.expect(owner.marker('OWNER_SURVIVES_REMOVE'))
        data(args.owner_host, 'session', 'share', identifier, '--team', args.team, '--all', '--role', 'viewer')
        data(args.owner_host, 'session', 'remove', identifier, peer_id)
        if pb(args.peer_host, '--workspace', args.team, 'session', 'join', identifier, timeout=40)[0] == 0:
            raise RuntimeError('peer kept access through the all-team grant after removal')
        data(args.owner_host, 'session', 'unshare', identifier)
        shared = False
    except Exception as error:
        failure = error
        if joined is not None:
            import re
            failure = RuntimeError(str(error) + ' | peer output: ' + re.sub(r'\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07\x1b]*(\x07|\x1b\\\\)', '', joined.output.decode(errors='replace'))[-400:])
    finally:
        if shared and identifier is not None:
            pb(args.owner_host, 'session', 'unshare', identifier, '--json')
        for terminal in (joined, owner):
            if terminal is not None:
                terminal.close()
        if identifier is not None:
            for action in ('close', 'delete'):
                code, out, _ = pb(args.owner_host, 'session', action, args.owner_target, identifier, '--json')
                try:
                    token = json.loads(out)['data']['confirmation_token']
                    pb(args.owner_host, 'session', action, args.owner_target, identifier, '--confirm', token, '--json')
                except Exception:
                    pass
                time.sleep(2)
    if failure is not None:
        print(json.dumps({'check': 'session-share-cross', 'status': 'FAIL', 'stage': STAGE[0], 'error': str(failure)}), flush=True)
        raise SystemExit(1)
    print(json.dumps({'check': 'session-share-cross', 'status': 'PASS', 'unshared_hidden': True,
                      'viewer_cannot_type': True, 'recent_output_replayed': True, 'interactive_types': True,
                      'remove_ends_access': True, 'all_team_grant_removed': True}), flush=True)


if __name__ == '__main__':
    main()
