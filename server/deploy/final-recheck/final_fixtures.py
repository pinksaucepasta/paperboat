#!/usr/bin/env python3
"""Server-side fixtures of the final re-check for one throwaway account.

  final_fixtures.py --email E [--member M ...] [--wait SECONDS]   create, print {"env": {...}}
  final_fixtures.py --email E --teardown [--member M ...]         remove, so the account can be deleted

The account must have signed in once (its users row exists). Create gives it a team it owns, with the paid
entitlement granted as test data (what a completed team checkout leaves: team-pro, quantity 10, an active period) and
the owner as its member, plus each --member (an account that has signed in once) as an active member. It is idempotent
and prints one JSON line the runner merges into the environment of every check: PAPERBOAT_SMOKE_TEAM is the team.
Teardown makes every other member inactive; a team left with its owner alone is deleted by the product itself when the
account is deleted (accountdeletion leaveTeamsTx), while a team with other members would block that deletion.

Like cleanup_test_account.py it runs SQL on the production database through `ssh <host> docker exec psql` (operator
access) and prints no secret. FINAL_FIXTURES_PSQL replaces that command (a local psql for the rehearsal).
"""
import argparse
import hashlib
import json
import os
import re
import shlex
import subprocess
import sys
import time

EMAIL = re.compile(r"^[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9.-]{1,190}$")
LOOKUP = "SELECT id FROM paperboat.users WHERE lower(primary_email)=lower(:'email') AND status='active'"

CREATE = """
\\set ON_ERROR_STOP on
BEGIN;
SELECT id AS owner FROM paperboat.users WHERE lower(primary_email)=lower(:'email') AND status='active' \\gset
INSERT INTO paperboat.teams(team_id, owner_account, generation, display_name, approve_new_machines)
  VALUES (:'team', :'owner', 1, :'team', true) ON CONFLICT (team_id) DO NOTHING;
INSERT INTO paperboat.team_members(team_id, account_id, membership_generation, role, active)
  VALUES (:'team', :'owner', 1, 'member', true) ON CONFLICT (team_id, account_id) DO NOTHING;
INSERT INTO paperboat.billing_workspaces(id, team_id, product_code, state, quantity, period_start, period_end, bandwidth_anchor, cancel_at_period_end)
  VALUES ('team:' || :'team', :'team', 'team-pro', 'active', 10, now() - interval '1 hour', now() + interval '90 days', now() - interval '1 hour', false)
  ON CONFLICT (id) DO UPDATE SET state='active', period_start=now() - interval '1 hour', period_end=now() + interval '90 days', cancel_at_period_end=false;
SELECT paperboat.team_paid_entitled(:'team') AS paid \\gset
\\if :paid
\\else
  \\echo team not entitled
  ROLLBACK;
  \\quit 3
\\endif
COMMIT;
"""
ADD_MEMBER = """
\\set ON_ERROR_STOP on
SELECT id AS member FROM paperboat.users WHERE lower(primary_email)=lower(:'member') AND status='active' \\gset
INSERT INTO paperboat.team_members(team_id, account_id, membership_generation, role, active)
  VALUES (:'team', :'member', 1, 'member', true)
  ON CONFLICT (team_id, account_id) DO UPDATE SET membership_generation=paperboat.team_members.membership_generation+1, role='member', active=true, joined_at=now();
"""
TEARDOWN = """
\\set ON_ERROR_STOP on
UPDATE paperboat.team_members m SET active=false, membership_generation=m.membership_generation+1
  FROM paperboat.teams t WHERE t.team_id=m.team_id AND t.team_id=:'team' AND m.account_id<>t.owner_account AND m.active;
SELECT count(*) FROM paperboat.team_members WHERE team_id=:'team' AND active;
"""


def team_name(email):
    """One stable team per account, so create is idempotent and teardown finds it."""
    return "paperboat-smoke-" + hashlib.sha256(email.lower().encode()).hexdigest()[:12]


# The server's connections run with search_path = paperboat; the triggers behind team_members use unqualified names.
SEARCH_PATH = "SET search_path TO paperboat, pg_catalog;\n"


def psql_command(variables):
    override = os.environ.get("FINAL_FIXTURES_PSQL")
    if override:
        base = shlex.split(override)
    else:
        base = ["ssh", os.environ.get("FINAL_FIXTURES_HOST", "root@coolify"), "docker", "exec", "-i", "paperboat-postgres-1", "psql", "-U", "postgres", "-d", "paperboat"]
    args = []
    for key, value in variables.items():
        args += ["-v", f"{key}={value}"]
    return base + ["-qAt"] + args


def run_sql(sql, variables):
    done = subprocess.run(psql_command(variables), input=SEARCH_PATH + sql, capture_output=True, text=True, timeout=120)
    if done.returncode != 0:
        raise SystemExit(f"sql step failed ({done.returncode}): {done.stderr.strip()[:300]}")
    return done.stdout


def account_exists(email):
    return run_sql(LOOKUP, {"email": email}).strip() != ""


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    parser.add_argument("--email", required=True)
    parser.add_argument("--member", action="append", default=[], help="another signed-in account to add as a team member")
    parser.add_argument("--teardown", action="store_true")
    parser.add_argument("--wait", type=int, default=60, help="seconds to wait for the account's first sign-in row")
    args = parser.parse_args(argv)
    for email in [args.email] + args.member:
        if not EMAIL.match(email):
            raise SystemExit(f"not an email address: {email!r}")
    team = team_name(args.email)
    if args.teardown:
        remaining = run_sql(TEARDOWN, {"team": team}).strip().splitlines()
        print(json.dumps({"teardown": {"team": team, "active_members": int(remaining[-1]) if remaining else 0}}))
        return 0
    deadline = time.monotonic() + args.wait
    for email in [args.email] + args.member:
        while not account_exists(email):
            if time.monotonic() >= deadline:
                raise SystemExit(f"{email} has no signed-in account yet: sign in once, then rerun")
            time.sleep(2)
    run_sql(CREATE, {"email": args.email, "team": team})
    for member in args.member:
        run_sql(ADD_MEMBER, {"team": team, "member": member})
    print(json.dumps({"env": {"PAPERBOAT_SMOKE_TEAM": team}}))
    return 0


if __name__ == "__main__":
    sys.exit(main())
