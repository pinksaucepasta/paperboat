#!/usr/bin/env python3
"""A scripted SCIM identity provider for the F19 directory checks (PLAN F19-05/06).

Acts as the IdP of a WorkOS "Custom SCIM v2.0" directory: it creates users and groups and changes
them the way Okta or Entra ID do (group members are added with PATCH, users are suspended with
PATCH active=false). The directory endpoint and bearer token come from DIR/endpoint and DIR/token
(mode 0600, written without printing them); they are never printed.

Users alice and bob (@DOMAIN), groups eng (alice) and finance (bob). Commands:
  seed                 create the users and groups and add the group members
  suspend|activate U   set user U inactive / active
  delete U             delete user U
  touch U              change U's family name (any change makes WorkOS send an event)
  users | groups       list them

Usage: scim_idp.py [--dir ~/lanes-scim] [--domain sso-test.pprbt.dev] COMMAND [USER]
"""
import argparse
import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request

USERS = {'alice': 'Alice', 'bob': 'Bob'}
GROUPS = {'eng': ['alice'], 'finance': ['bob']}


class Directory:
    def __init__(self, path):
        self.endpoint = open(os.path.join(path, 'endpoint')).read().strip()
        self.token = open(os.path.join(path, 'token')).read().strip()

    def call(self, method, path, body=None):
        req = urllib.request.Request(self.endpoint + path, method=method,
                                     data=json.dumps(body).encode() if body is not None else None,
                                     headers={'Authorization': 'Bearer ' + self.token, 'Content-Type': 'application/scim+json'})
        try:
            with urllib.request.urlopen(req, timeout=30) as r:
                raw = r.read()
                return r.status, json.loads(raw) if raw else None
        except urllib.error.HTTPError as e:
            return e.code, e.read().decode(errors='replace')[:300]

    def find(self, kind, attr, value):
        s, d = self.call('GET', '/%s?filter=%s' % (kind, urllib.parse.quote('%s eq "%s"' % (attr, value))))
        return (d.get('Resources') or [None])[0] if s == 200 else None

    def patch(self, kind, ident, op, path, value):
        return self.call('PATCH', '/%s/%s' % (kind, ident), {'schemas': ['urn:ietf:params:scim:api:messages:2.0:PatchOp'],
                                                            'Operations': [{'op': op, 'path': path, 'value': value}]})[0]


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument('--dir', default=os.path.expanduser('~/lanes-scim'))
    p.add_argument('--domain', default='sso-test.pprbt.dev')
    p.add_argument('command')
    p.add_argument('user', nargs='?')
    a = p.parse_args()
    d = Directory(a.dir)
    email = lambda u: '%s@%s' % (u, a.domain)
    user = lambda u: d.find('Users', 'userName', email(u))
    if a.command == 'seed':
        ids = {}
        for u, first in USERS.items():
            cur = user(u)
            if not cur:
                s, cur = d.call('POST', '/Users', {'schemas': ['urn:ietf:params:scim:schemas:core:2.0:User'], 'userName': email(u),
                    'externalId': 'f19-' + u, 'name': {'givenName': first, 'familyName': 'F19'},
                    'emails': [{'value': email(u), 'primary': True, 'type': 'work'}], 'active': True})
                print('user', u, s)
            ids[u] = cur['id']
        for g, members in GROUPS.items():
            cur = d.find('Groups', 'displayName', g)
            if not cur:
                s, cur = d.call('POST', '/Groups', {'schemas': ['urn:ietf:params:scim:schemas:core:2.0:Group'], 'displayName': g, 'externalId': 'f19-' + g})
                print('group', g, s)
            # Like Okta: members are added with PATCH (WorkOS ignores members sent on create).
            print('members', g, d.patch('Groups', cur['id'], 'add', 'members', [{'value': ids[m]} for m in members]))
    elif a.command in ('suspend', 'activate'):
        print(a.command, a.user, d.patch('Users', user(a.user)['id'], 'replace', 'active', a.command == 'activate'))
    elif a.command == 'delete':
        print('delete', a.user, d.call('DELETE', '/Users/' + user(a.user)['id'])[0])
    elif a.command == 'touch':
        print('touch', a.user, d.patch('Users', user(a.user)['id'], 'replace', 'name.familyName', 'F19-%d' % time.time()))
    elif a.command in ('users', 'groups'):
        s, body = d.call('GET', '/' + a.command.capitalize())
        print(s, [(r.get('userName') or r.get('displayName'), r.get('active')) for r in (body or {}).get('Resources', [])] if s == 200 else body)
    else:
        p.error('unknown command')


if __name__ == '__main__':
    main()
