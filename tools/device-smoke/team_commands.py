#!/usr/bin/env python3
"""The single-account `pb team` commands against the retained paid team (F12-02, C192-C206, C213-C215 refusals).

Runs on an enrolled machine whose account owns TEAM (--team or PAPERBOAT_SMOKE_TEAM). It never changes the team's
members: it lists and shows the team, prints the Checkout link of `pb team create --no-wait` (nothing exists until
paid), invites an email that has no Paperboat account, finds that invitation with `invitations` and `invitation`,
cancels it, and checks that the commands which need a second member (accept, decline, leave, remove, role, transfer,
delete) refuse this account's own call with a nonzero exit. The two-account success paths are team_lifecycle.py.
"""
import argparse
import json
import os
from pathlib import Path
import secrets
import subprocess

from install_lifecycle import registration
from pairing_lifecycle import command_result, invoke


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--team', default=os.environ.get('PAPERBOAT_SMOKE_TEAM', ''))
    args = parser.parse_args()
    if not args.team:
        raise RuntimeError('--team or PAPERBOAT_SMOKE_TEAM names the retained paid team')
    pb = Path.home() / '.local/bin/pb'
    before = registration()

    def call(*words):
        value = json.loads(invoke(pb, *words, raw=True, input='', timeout=60))
        return value.get('data', value) if isinstance(value, dict) else value

    def refused(*words):
        result = command_result(pb, *words, timeout=60, input='')
        assert result.returncode != 0, 'accepted: pb ' + ' '.join(words[:2])

    me = call('auth', 'status', '--json')['account']['id']
    assert me == before['account_id']
    help_text = invoke(pb, 'team', raw=True, input='', timeout=30)
    assert all(word in help_text for word in ('invite', 'create', 'accept', 'transfer'))
    team = call('team', 'get', args.team, '--json')
    assert team['team_id'] == args.team and team['owner_account'] == me and team['generation'] >= 1
    assert any(t['team_id'] == args.team for t in call('team', 'list', '--json'))
    created = call('team', 'create', 'smoke-' + secrets.token_hex(6), '--no-wait', '--json')
    assert created['checkout_url'].startswith('https://'), 'team create printed no Checkout link'
    invitation = None
    try:
        email = 'smoke-%s@example.test' % secrets.token_hex(6)
        item = call('team', 'invite', args.team, email, '--generation', str(team['generation']), '--json')
        invitation = item['invitation_id']
        assert item['email'] == email and item['team_id'] == args.team
        assert any(i['invitation_id'] == invitation for i in call('team', 'invitations', args.team, '--json'))
        shown = call('team', 'invitation', invitation)
        assert shown['team_id'] == args.team and shown['state'] == 'pending' and shown['email'] == email
        refused('team', 'accept', invitation, '--json')
        refused('team', 'decline', invitation, '--json')
        generation = str(call('team', 'get', args.team, '--json')['generation'])
        call('team', 'cancel-invite', args.team, invitation, '--generation', generation, '--json')
        invitation = None
        assert call('team', 'invitation', item['invitation_id'])['state'] == 'cancelled'
        stranger = 'user_' + secrets.token_hex(8)
        refused('team', 'leave', args.team, '--generation', generation, '--json')
        refused('team', 'transfer', args.team, me, '--generation', generation, '--json')
        refused('team', 'role', args.team, stranger, 'admin', '--generation', generation, '--json')
        refused('team', 'remove', args.team, stranger, '--generation', generation, '--json')
        refused('team', 'delete', args.team, '--generation', generation, '--confirm', 'not-' + args.team, '--json')
    finally:
        if invitation:
            current = call('team', 'get', args.team, '--json')['generation']
            subprocess.run([str(pb), 'team', 'cancel-invite', args.team, invitation, '--generation', str(current), '--json'],
                           capture_output=True, timeout=60)
    assert call('team', 'get', args.team, '--json')['owner_account'] == me
    assert registration() == before
    print(json.dumps({'check': 'team-commands', 'status': 'PASS', 'get_list_create_checkout': True,
                      'invite_without_account_find_show_cancel': True, 'second_member_commands_refuse_alone': True,
                      'enrollment_preserved': True}))


if __name__ == '__main__':
    main()
