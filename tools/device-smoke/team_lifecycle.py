#!/usr/bin/env python3
"""Qualify real owner/teammate team commands without changing device enrollment."""
import argparse
import json
from pathlib import Path
import secrets

from install_lifecycle import registration
from pairing_lifecycle import invoke
from teammate_profile import environment, profile_root


def check_roles(call, current, invite, team_id, owner_id, recipient_id):
    def mutate(action, *values, teammate=False):
        return call('team', action, team_id, *values, '--generation',
                    str(current()['generation']), '--json', teammate=teammate)

    def member():
        return next(m for m in current()['members'] if m['account_id'] == recipient_id)

    for role in ['admin', 'member']:
        mutate('role', recipient_id, role)
        assert member()['active'] and member()['role'] == role
    mutate('remove', recipient_id)
    assert not member()['active']
    call('team', 'accept', invite(), '--json', teammate=True)
    assert member()['active']
    mutate('leave', teammate=True)
    assert not member()['active']
    call('team', 'accept', invite(), '--json', teammate=True)
    mutate('transfer', recipient_id)
    assert current()['owner_account'] == recipient_id
    mutate('transfer', owner_id, teammate=True)
    assert current()['owner_account'] == owner_id


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--roles', action='store_true')
    args = parser.parse_args()
    pb = Path.home() / '.local/bin/pb'
    root = profile_root()
    config = root / 'config.json'
    if not config.exists():
        raise RuntimeError('controlled teammate profile needs browser sign-in first')
    env = environment(root)

    def call(*args, teammate=False):
        prefix = ['--config', str(config)] if teammate else []
        # Team inventories are canonical arrays, unlike command envelopes.
        value = json.loads(invoke(pb, *prefix, *args, raw=True, input='',
                                  env=env if teammate else None, timeout=40))
        return value.get('data', value) if isinstance(value, dict) else value

    before = registration()
    owner = call('auth', 'status', '--json')
    recipient = call('auth', 'status', '--json', teammate=True)
    assert owner['signed_in'] and recipient['signed_in']
    assert owner['account']['id'] == before['account_id']
    assert recipient['account']['id'] != owner['account']['id']
    email = recipient['account']['email']
    team_id = 'smoke-team-' + secrets.token_hex(8)
    state = root / 'owned-team.json'
    state.write_text(json.dumps({'team_id': team_id, 'owner': before['account_id']}))
    state.chmod(0o600)
    created = False
    primary_error = None
    text = invoke(pb, 'team', raw=True, input='', timeout=15)
    assert 'invite' in text and 'create' in text
    try:
        team = call('team', 'create', team_id, '--json')
        created = True
        assert team['team_id'] == team_id and team['owner_account'] == before['account_id']
        assert any(t['team_id'] == team_id for t in call('team', 'list', '--json'))

        def current():
            return call('team', 'get', team_id, '--json')

        def invite():
            item = call('team', 'invite', team_id, email,
                        '--generation', str(current()['generation']), '--json')
            assert item['email'] == email
            assert any(i['invitation_id'] == item['invitation_id'] for i in
                       call('team', 'invitations', '--json', teammate=True))
            shown = call('team', 'invitation', item['invitation_id'], teammate=True)
            assert shown['team_id'] == team_id and shown['state'] == 'pending'
            return item['invitation_id']

        invitation = invite()
        declined = call('team', 'decline', invitation, '--json', teammate=True)
        assert declined['state'] == 'cancelled'
        invitation = invite()
        call('team', 'cancel-invite', team_id, invitation,
             '--generation', str(current()['generation']), '--json')
        assert call('team', 'invitation', invitation)['state'] == 'cancelled'
        invitation = invite()
        accepted = call('team', 'accept', invitation, '--json', teammate=True)
        assert accepted['team_id'] == team_id
        assert any(m['account_id'] == recipient['account']['id'] and m['active']
                   for m in current()['members'])
        assert any(t['team_id'] == team_id for t in
                   call('team', 'list', '--json', teammate=True))
        if args.roles:
            check_roles(call, current, invite, team_id, before['account_id'],
                        recipient['account']['id'])
    except BaseException as error:
        primary_error = error
        raise
    finally:
        try:
            if created:
                current_team = call('team', 'get', team_id, '--json')
                cleanup_owner = current_team['owner_account']
                assert cleanup_owner in (before['account_id'], recipient['account']['id'])
                deleted = call('team', 'delete', team_id, '--generation',
                               str(current_team['generation']), '--confirm', team_id, '--json',
                               teammate=cleanup_owner == recipient['account']['id'])
                assert deleted['deleted']
                assert not any(t['team_id'] == team_id for t in call('team', 'list', '--json'))
            assert registration() == before
            assert call('auth', 'status', '--json')['account']['id'] == before['account_id']
            state.unlink()
        except BaseException:
            if primary_error is None:
                raise
            print(json.dumps({'check': 'team-lifecycle', 'cleanup': 'failed',
                              'owned_state_retained': True}), flush=True)
    print(json.dumps({'check': 'team-roles' if args.roles else 'team-lifecycle', 'status': 'PASS',
                      'create_get_list_delete': True, 'invite_accept_decline_cancel': True,
                      'roles_remove_leave_transfer': args.roles,
                      'independent_verified_recipient': True, 'enrollment_preserved': True}))


if __name__ == '__main__':
    main()
