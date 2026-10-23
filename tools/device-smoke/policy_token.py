#!/usr/bin/env python3
"""F18-06 API token scope on real machines, run from the control host.

OWNER runs pb signed in as the token's creator (OWNER_CMD); TOKEN runs pb on another machine of the
same account that this check signs in with the token (TOKEN_CMD; its CLI session is replaced and is
signed out at the end, when the token is revoked). The check creates a token scoped to `exec` on
--allowed, signs TOKEN in with `pb login --api-token-file -`, and checks that exec on --allowed works,
exec on --other is refused as outside the token's scope, a write outside the scope (creating
another API token) is refused, and revoking the token ends the session. The secret only passes
from OWNER's stdout to TOKEN's stdin; it is never printed. Then the solo case: a token that may
only exec on tag:ci works on the tagged machine, its use is attributed to it, and removing the tag
(a policy change) takes the access from the token.

Usage: policy_token.py --allowed lk-a --other lk-b \
         --owner-cmd 'ssh pbvm2 sudo docker exec -i -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 lk-a' \
         --token-cmd 'ssh pbvm2 sudo docker exec -i -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 lk-b'
"""
import argparse
import json
import random
import re
import shlex
import string
import subprocess
import sys
import time


def sh(prefix, pb, args, timeout=150, stdin=None):
    """Runs pb on one side: prefix is the ssh/docker command line, quoted once for its remote shell."""
    remote = ' '.join(shlex.quote(x) for x in ['timeout', '120', pb] + list(args))
    done = subprocess.run(shlex.split(prefix) + [remote], capture_output=True, text=True, timeout=timeout, input=stdin)
    return done.returncode, done.stdout, done.stderr


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--allowed', required=True)
    p.add_argument('--other', required=True)
    p.add_argument('--owner-cmd', required=True)
    p.add_argument('--token-cmd', required=True)
    p.add_argument('--pb', default='/home/pbt/.local/bin/pb')
    a = p.parse_args()
    owner = lambda *args, **kw: sh(a.owner_cmd, a.pb, args, **kw)
    holder = lambda *args, **kw: sh(a.token_cmd, a.pb, args, **kw)
    fails = []

    def step(name, ok, detail=''):
        detail = re.sub(r'pbat_[A-Za-z0-9_-]+', 'pbat_<redacted>', detail)
        print(('PASS ' if ok else 'FAIL ') + name + ('' if ok else ': ' + detail.strip()[-400:]))
        if not ok:
            fails.append(name)

    name = 'f18-scope-' + ''.join(random.choices(string.ascii_lowercase, k=6))
    marker = name + '-ran'
    scope = json.dumps([{'dst': ['machine:' + a.allowed], 'allow': ['exec']}])
    rc, out, err = owner('api-token', 'create', name, '--expires', '1d', '--scope', scope, '--json')
    secret = ''
    try:
        d = json.loads(out)
        d = d.get('data', d)
        secret = d['secret']
        ok = d['api_token']['scope'] == [{'dst': ['machine:' + a.allowed], 'allow': ['exec']}]
    except (ValueError, KeyError, TypeError):
        ok = False
    step('create a token scoped to exec on ' + a.allowed, ok and secret.startswith('pbat_'), err)
    if not secret:
        return 1
    try:
        rc, out, err = holder('login', '--api-token-file', '-', stdin=secret + '\n')
        step('pb login --api-token-file signs in with the token', rc == 0 and 'API token' in out, out + err)
        secret = ''
        # Access reaches the peer networks asynchronously: retry the allowed exec, bounded.
        for attempt in range(9):
            rc, out, err = holder('exec', a.allowed, '--', 'echo', marker)
            if rc == 0 and marker in out:
                break
            time.sleep(10)
        step('exec on the scoped machine works', rc == 0 and marker in out, out + err)
        rc, out, err = holder('exec', a.other, '--', 'echo', marker)
        step('exec on another machine is outside the scope', rc != 0 and marker not in out and 'scope' in (out + err).lower(), out + err)
        rc, out, err = holder('api-token', 'create', name + '-x', '--json')
        if rc == 0:
            owner('api-token', 'revoke', name + '-x', '--yes')
        step('a write outside the scope is refused', rc != 0 and 'outside_token_scope' in out + err, out + err)
    finally:
        rc, out, err = owner('api-token', 'revoke', name, '--yes')
        step('revoke the token', rc == 0, out + err)
    rc, out, err = holder('machine', 'list', '--json')
    step('revoking ends the token session', rc != 0, out + err)
    tag_scope(a, owner, holder, step, name + '-ci', marker)
    print('RESULT', 'PASS' if not fails else 'FAIL ' + ','.join(fails))
    return 1 if fails else 0


def tag_scope(a, owner, holder, step, name, marker):
    """The solo case: a token that may only exec on tag:ci. Its use is attributed to it (last_used_at),
    and a rule change that takes the tag from the machine takes the access from the token at once."""
    rc, out, err = owner('policy', 'get', '--workspace', 'personal', '--json')
    try:
        cur = data(out)
        start, doc = cur['version'], json.loads(cur['policy'])
    except (ValueError, KeyError, TypeError):
        step('read the personal policy', False, out + err)
        return
    doc.setdefault('tags', {})['tag:ci'] = ['machine:' + a.allowed]
    rc, out, err = owner('policy', 'set', '-', '--workspace', 'personal', '--message', name, '--yes', stdin=json.dumps(doc))
    step('tag the scoped machine tag:ci in the personal policy', rc == 0, out + err)
    if rc != 0:
        return
    secret = ''
    try:
        rc, out, err = owner('api-token', 'create', name, '--expires', '1d', '--scope', json.dumps([{'dst': ['tag:ci'], 'allow': ['exec']}]), '--json')
        try:
            secret = data(out)['secret']
        except (ValueError, KeyError, TypeError):
            pass
        step('create a token that may only exec on tag:ci', secret.startswith('pbat_'), err)
        if not secret:
            return
        rc, out, err = holder('login', '--api-token-file', '-', stdin=secret + '\n')
        secret = ''
        step('sign in with the tag:ci token', rc == 0, out + err)
        for attempt in range(9):
            rc, out, err = holder('exec', a.allowed, '--', 'echo', marker)
            if rc == 0 and marker in out:
                break
            time.sleep(10)
        step('exec on the tag:ci machine works', rc == 0 and marker in out, out + err)
        rc, out, err = owner('api-token', 'list', '--workspace', 'personal', '--json')
        used = [t for t in (data(out).get('api_tokens') or []) if t.get('name') == name and t.get('last_used_at')] if rc == 0 else []
        step('the use is attributed to the token (last_used_at)', len(used) == 1, out[-300:] + err)
    finally:
        rc, out, err = owner('policy', 'restore', str(start), '--workspace', 'personal', '--message', name + ' cleanup', '--yes')
        step('restore the personal policy (the machine loses tag:ci)', rc == 0, out + err)
    rc, out, err = holder('exec', a.allowed, '--', 'echo', marker)
    step('without the tag the token may not exec there', rc != 0 and marker not in out and 'scope' in (out + err).lower(), out + err)
    rc, out, err = owner('api-token', 'revoke', name, '--yes')
    step('revoke the tag:ci token', rc == 0, out + err)


def data(out):
    d = json.loads(out)
    return d.get('data', d) if isinstance(d, dict) else d


if __name__ == '__main__':
    sys.exit(main())
