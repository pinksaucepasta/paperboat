#!/usr/bin/env python3
"""The current bandwidth period of ONE throwaway account's personal workspace, for billing_checks.py (as the hand proofs did).

usage: workspace_usage.py USER_ID show | cap | spend BYTES | restore CONSUMED OBSERVED
  cap      consumed_bytes = included_bytes: the fair-use amount is used up, without moving 5 GB through a relay
  spend    consumed_bytes = observed_bytes = BYTES: traffic already spent, for the check that a plan change keeps it
  restore  puts both counters back
Prints one JSON line {"included","consumed","observed","before":[consumed,observed]}.
It writes to the rehearsal database through `ssh <database_host> runuser -u postgres psql` (operator access), and only for an
account that fresh_account.mjs made: its address matches final-<10 hex>@ and it is under six hours old. Exactly one period row
must match, else nothing is changed.
"""
import json
import re
import subprocess
import sys

from dev_targets import load

# the fence: nothing but a throwaway account of this harness can be read or written
SELECT = """SELECT p.workspace_id, p.period_start, p.period_end, p.included_bytes, p.consumed_bytes, p.observed_bytes
FROM paperboat.billing_workspace_usage_periods p JOIN paperboat.users u ON p.workspace_id = 'personal:' || u.id
WHERE u.id = '%(user)s' AND u.primary_email ~ '^final-[0-9a-f]{10}@' AND u.created_at > now() - interval '6 hours'
  AND p.period_start <= now() AND p.period_end > now() AND p.included_bytes IS NOT NULL
FOR UPDATE OF p"""


def script(user, action, values=()):
    """The one transaction: lock the account's current period, change it when asked, print before and after."""
    if not re.fullmatch(r'user_[0-9a-f-]{36}', user):
        raise ValueError('USER_ID must be user_<uuid>')
    numbers = [int(v) for v in values]
    if any(n < 0 or n > 10**12 for n in numbers):
        raise ValueError('byte counts must be between 0 and 1e12')
    change = {'show': None, 'cap': 'consumed_bytes = p.included_bytes',
              'spend': 'consumed_bytes = %d, observed_bytes = %d' % (tuple(numbers) * 2) if action == 'spend' and len(numbers) == 1 else None,
              'restore': 'consumed_bytes = %d, observed_bytes = %d' % tuple(numbers) if action == 'restore' and len(numbers) == 2 else None}
    if action not in change or (action != 'show' and change[action] is None) or (action in ('show', 'cap') and numbers):
        raise ValueError(__doc__)
    sql = ['\\set ON_ERROR_STOP 1', 'BEGIN;', 'CREATE TEMP TABLE chosen ON COMMIT DROP AS ' + SELECT % {'user': user} + ';',
           'DO $$ BEGIN IF (SELECT count(*) FROM chosen) <> 1 THEN RAISE EXCEPTION '
           "'not exactly one current period of a throwaway account'; END IF; END $$;"]
    if change[action]:
        sql.append('UPDATE paperboat.billing_workspace_usage_periods p SET %s FROM chosen c WHERE p.workspace_id = c.workspace_id '
                   'AND p.period_start = c.period_start AND p.period_end = c.period_end;' % change[action])
    sql.append("SELECT json_build_object('included', p.included_bytes, 'consumed', p.consumed_bytes, 'observed', p.observed_bytes, "
               "'before', json_build_array(c.consumed_bytes, c.observed_bytes)) FROM paperboat.billing_workspace_usage_periods p "
               'JOIN chosen c ON p.workspace_id = c.workspace_id AND p.period_start = c.period_start AND p.period_end = c.period_end;')
    return '\n'.join(sql + ['COMMIT;']) + '\n'


def run(user, action, *values, host=None):
    host = host or load().get('database_host')
    if not host:
        raise RuntimeError('set database_host in the local development targets')
    done = subprocess.run(['ssh', '-o', 'BatchMode=yes', '--', host, 'runuser', '-u', 'postgres', '--', 'psql', '-d', 'paperboat', '-qAt'],
                          input=script(user, action, values), capture_output=True, text=True, timeout=120)
    if done.returncode != 0 or 'ERROR' in done.stderr:
        raise RuntimeError('workspace usage %s failed: %s' % (action, done.stderr.strip()[-300:]))
    return json.loads(done.stdout.strip().splitlines()[-1])


if __name__ == '__main__':
    if len(sys.argv) < 3:
        sys.exit(__doc__)
    print(json.dumps(run(*sys.argv[1:])))
