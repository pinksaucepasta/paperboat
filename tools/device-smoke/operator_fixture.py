#!/usr/bin/env python3
"""Operator-side smoke fixture: the retained paid team keeps a running paid plan and the SSO and SCIM add-ons.

usage: operator_fixture.py status|ensure [--team TEAM] [--cred FILE] [--api URL] [--days N]
The operator credential file is forward_live.py's (line 1 email, 2 password, 3 authenticator key; mode 600;
default ~/.config/paperboat-dev/operator-cred). `ensure` is idempotent: it comps a team plan (the admin plan grant, longest 366 days)
when the team has none running or it ends within --renew-days, and turns the SSO and SCIM entitlements on (no end date) when they are not.
A Stripe subscription is never touched (the grant is refused while one is live). Prints facts, never a secret.
"""
import argparse
import datetime
import json
import os
from pathlib import Path
import sys
import time
import urllib.error
import urllib.request
import uuid

import dev_targets
import forward_live

CRED = Path.home() / '.config/paperboat-dev/operator-cred'
PLAN = 'team-year'  # the comp plan: the longest-running team product
SEATS = 10


class Operator(forward_live.Operator):
    def write(self, method, path, body, target):
        """An operator write: target, reason reference and idempotency key are required headers."""
        headers = {'Content-Type': 'application/json', 'X-CSRF-Token': self.csrf, 'Operator-Target': target,
                   'Operator-Reason': 'smoke-fixture', 'Idempotency-Key': str(uuid.uuid4())}
        request = urllib.request.Request(self.api + path, method=method, headers=headers, data=json.dumps(body).encode())
        try:
            with self.opener.open(request, timeout=30) as response:
                return response.status, json.loads(response.read() or b'null')
        except urllib.error.HTTPError as error:
            return error.code, (json.loads(error.read() or b'null') if error.headers.get_content_type() == 'application/json' else None)


def facts(op, team):
    """(workspace, entitlement grants) of the team, as the operator API reports them."""
    workspace = f'team:{team}'
    status, plan = op.call('GET', f'/v1/admin/billing/workspaces/{workspace}')
    if status != 200:
        raise forward_live.Fail(f'reading the plan of {workspace} failed (HTTP {status})')
    status, ent = op.call('GET', f'/v1/admin/billing/workspaces/{workspace}/entitlements')
    if status != 200:
        raise forward_live.Fail(f'reading the entitlements of {workspace} failed (HTTP {status})')
    return plan['data'], ent['data']['grants']


def running(plan, days):
    """The plan is paid and runs at least `days` more days."""
    if plan.get('state') not in ('active', 'trialing', 'granted') or not plan.get('period_end'):
        return False
    end = datetime.datetime.fromisoformat(plan['period_end'].replace('Z', '+00:00'))
    return end - datetime.datetime.now(datetime.timezone.utc) >= datetime.timedelta(days=days)


def addons_on(grants):
    return {g['kind'] for g in grants if g.get('running') and g['kind'] in ('sso', 'scim')}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.split('\n')[0])
    parser.add_argument('action', choices=('status', 'ensure'))
    parser.add_argument('--team', default=dev_targets.smoke_environment(dev_targets.load()).get('PAPERBOAT_SMOKE_TEAM', 'paperboat-smoke-paid'))
    parser.add_argument('--cred', default=str(CRED))
    parser.add_argument('--api', default=dev_targets.smoke_environment(dev_targets.load()).get('SMOKE_OPERATOR_API'), required='SMOKE_OPERATOR_API' not in dev_targets.smoke_environment(dev_targets.load()),
                        help='the API origin (SMOKE_OPERATOR_API in targets.json)')
    parser.add_argument('--renew-days', type=int, default=30)
    parser.add_argument('--days', type=int, default=366)
    args = parser.parse_args(argv)
    op = Operator(args.api, args.cred)
    try:
        op.login()
    except forward_live.Fail:  # an authenticator code works once: a login a moment ago used this step's and the next one's
        time.sleep(31)
        op.login()
    plan, grants = facts(op, args.team)
    state = {'state': plan.get('state'), 'ends': plan.get('period_end'), 'seats': plan.get('quantity'), 'addons': sorted(addons_on(grants))}
    print(json.dumps(state))
    if args.action == 'status':
        return 0 if running(plan, args.renew_days) and addons_on(grants) == {'sso', 'scim'} else 1
    target = f'team:{args.team}'
    if not running(plan, args.renew_days):
        status, answer = op.write('POST', f'/v1/admin/billing/workspaces/{target}/grant',
                                  {'revision': plan['revision'], 'product_code': PLAN, 'days': args.days, 'quantity': SEATS}, target)
        if status != 200:
            raise forward_live.Fail(f'granting the {PLAN} plan to {target} failed (HTTP {status}): {str(answer)[:200]}')
    for kind in ('sso', 'scim'):
        if kind not in addons_on(grants):
            status, answer = op.write('POST', f'/v1/admin/billing/workspaces/{target}/entitlements', {'entitlement': kind, 'amount': 1}, target)
            if status not in (200, 201):
                raise forward_live.Fail(f'granting {kind} to {target} failed (HTTP {status}): {str(answer)[:200]}')
    plan, grants = facts(op, args.team)
    print(json.dumps({'state': plan.get('state'), 'ends': plan.get('period_end'), 'seats': plan.get('quantity'), 'addons': sorted(addons_on(grants))}))
    return 0 if running(plan, args.renew_days) and addons_on(grants) == {'sso', 'scim'} else 1


if __name__ == '__main__':
    try:
        sys.exit(main())
    except forward_live.Fail as error:
        print('operator_fixture:', error, file=sys.stderr)
        sys.exit(2)
