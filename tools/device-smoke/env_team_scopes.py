#!/usr/bin/env python3
"""F10-04: team ENV scope lifecycle with two real accounts; access follows grants and roles.

OWNER is a paired machine of the team owner (an unlocked-able ENV vault), MEMBER a paired machine of a second account
(its own ENV vault). Both are commands that run pb, e.g. `sudo -n docker exec -u pbt -e HOME=/home/pbt
-e XDG_RUNTIME_DIR=/run/user/1000 CT /home/pbt/.local/bin/pb`. The team must be paid, with a free seat.

Usage: env_team_scopes.py --team TEAM --owner-cmd CMD --owner-password-file F --member-cmd CMD --member-password-file F
         --member-account ACCOUNT_ID --member-email EMAIL
  1. the member joins the team (invited by email, accepted); the owner resets the team scope: no values
  2. the owner sets a team value; before any grant the member cannot list the scope
  3. env_read + `pb env team grant` + the member's `pb env grants sync`: the member lists the value's name (same revision)
  4. roles: the member (read only) cannot set a team value, rotate or reset
  5. rotate preserves the value for the owner and, after the member's sync, for the member at the new key epoch
  6. revoke (`pb env team revoke --remove MEMBER`, which also removes the member): the member's sync does not give the
     new key; the owner still lists it
  7. reset discards the value; the member is out of the team again (removed here when a step failed before step 6)
The value is random, never printed and never passed on a command line.
"""
import argparse
import json
import shlex
import subprocess
import sys
import uuid


class Failure(Exception):
    pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for flag in ('team', 'owner-cmd', 'owner-password-file', 'member-cmd', 'member-password-file', 'member-account', 'member-email'):
        parser.add_argument('--' + flag, required=True)
    args = parser.parse_args()
    team = args.team
    results = {}

    def pb(command, *arguments, timeout=180, stdin=None):
        argv = shlex.split(command)
        if stdin is not None and 'exec' in argv:
            argv.insert(argv.index('exec') + 1, '-i')  # docker passes stdin only with -i
        done = subprocess.run(argv + list(arguments), input=stdin, capture_output=True, text=True, timeout=timeout,
                              **({} if stdin is not None else {'stdin': subprocess.DEVNULL}))
        try:
            body = json.loads(done.stdout) if done.stdout.strip() else {}
        except ValueError:
            body = {}
        return done.returncode, body, done.stderr

    owner = lambda *a, **k: pb(args.owner_cmd, *a, **k)
    member = lambda *a, **k: pb(args.member_cmd, *a, **k)

    def must(result, what):
        code, body, err = result
        if code:
            raise Failure('%s failed: %s' % (what, (body.get('error', {}).get('message') if isinstance(body, dict) else '') or err[:120]))
        return body

    def team_state():
        body = must(owner('team', 'get', team, '--json'), 'team get')
        return body.get('data', body)

    def scope(command):
        code, body, _ = pb(command, '--workspace', team, 'env', 'list', '--team', team, '--json')
        return body.get('data') if code == 0 and isinstance(body, dict) else None

    def sync_member():
        must(member('env', 'grants', 'sync', '--json'), 'member grants sync')

    name = 'PB_TEAM_' + uuid.uuid4().hex[:8].upper()
    joined = granted = False
    try:
        for command, password in ((args.owner_cmd, args.owner_password_file), (args.member_cmd, args.member_password_file)):
            must(pb(command, 'env', 'vault', 'unlock', '--password-file', password, '--json'), 'vault unlock')
        if not any(m['account_id'] == args.member_account and m['active'] for m in team_state()['members']):
            must(owner('team', 'invite', team, args.member_email, '--generation', str(team_state()['generation']), '--json'), 'invite')
            invitations = must(member('team', 'invitations', '--json'), 'member invitations')
            invitation = next(i['invitation_id'] for i in invitations if i['team_id'] == team)
            must(member('team', 'accept', invitation, '--json'), 'accept')
        joined = True
        must(owner('env', 'team', 'reset', team, '--confirm', 'RESET ENV TEAM ' + team, '--json'), 'reset')
        results['reset_creates_an_empty_scope'] = scope(args.owner_cmd)['names'] == []

        value = uuid.uuid4().hex
        must(owner('--workspace', team, 'env', 'set', name, '--team', team, '--value-stdin', '--json', stdin=value), 'owner set')
        first = scope(args.owner_cmd)
        if first['names'] != [name]:
            raise Failure('the owner does not list the team value')
        if scope(args.member_cmd) is not None:
            raise Failure('the member listed the team scope before any grant')
        results['no_access_before_grant'] = True

        must(owner('team', 'grant', team, args.member_account, 'env', team, 'read', '--generation', str(team_state()['generation']), '--json'), 'env_read grant')
        granted = True
        must(owner('env', 'team', 'grant', team, args.member_account, '--json'), 'env team grant')
        sync_member()
        listed = scope(args.member_cmd)
        if not listed or listed['names'] != [name] or listed['revision'] != first['revision']:
            raise Failure('after grant and sync the member does not list the value at the owner\'s revision')
        results['grant_and_sync_give_access'] = True

        if member('--workspace', team, 'env', 'set', name + '_X', '--team', team, '--value-stdin', '--json', stdin='denied')[0] == 0:
            raise Failure('a read-only member set a team value')
        for verb in ('rotate', 'reset'):
            if member('env', 'team', verb, team, '--confirm', ('ROTATE' if verb == 'rotate' else 'RESET') + ' ENV TEAM ' + team, '--json')[0] == 0:
                raise Failure('a member could %s the team scope' % verb)
        results['roles_are_enforced'] = True

        must(owner('env', 'team', 'rotate', team, '--confirm', 'ROTATE ENV TEAM ' + team, '--json'), 'rotate')
        rotated = scope(args.owner_cmd)
        if rotated['names'] != [name] or rotated['key_epoch'] <= first['key_epoch']:
            raise Failure('rotation lost the value or kept the key epoch')
        sync_member()
        after = scope(args.member_cmd)
        if not after or after['names'] != [name] or after['key_epoch'] != rotated['key_epoch']:
            raise Failure('the member does not follow the rotation after a sync')
        results['rotate_preserves_values'] = True

        must(owner('env', 'team', 'revoke', team, '--remove', args.member_account, '--confirm', 'ROTATE ENV TEAM ' + team, '--json'), 'revoke')
        joined = granted = False  # revoking removes the member and its grants
        revoked = scope(args.owner_cmd)
        sync_member()
        stale = scope(args.member_cmd)
        if revoked['names'] != [name] or (stale and stale['key_epoch'] == revoked['key_epoch']):
            raise Failure('revoke lost the owner\'s value or the member still reads the new key epoch')
        results['revoke_fences_the_member'] = True

        must(owner('env', 'team', 'reset', team, '--confirm', 'RESET ENV TEAM ' + team, '--json'), 'final reset')
        if scope(args.owner_cmd)['names'] != []:
            raise Failure('reset left the value')
        results['reset_discards_values'] = True
    finally:
        cleanup = []  # a cleanup failure must not hide the primary one
        try:
            if granted:
                cleanup.append(owner('team', 'grant', team, args.member_account, 'env', team, 'read', '--active=false', '--generation', str(team_state()['generation']), '--json'))
            if joined:
                cleanup.append(owner('team', 'remove', team, args.member_account, '--generation', str(team_state()['generation']), '--json'))
        except Exception as err:
            print('cleanup failed: %s' % err, file=sys.stderr)
        results['cleanup_ok'] = all(code == 0 for code, _, _ in cleanup)
    print(json.dumps({'check': 'env-team-scopes', 'status': 'PASS', **results}), flush=True)


if __name__ == '__main__':
    try:
        main()
    except Failure as err:
        print(json.dumps({'check': 'env-team-scopes', 'status': 'FAIL', 'detail': str(err)}))
        sys.exit(1)
