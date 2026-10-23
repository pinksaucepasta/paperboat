#!/usr/bin/env python3
"""F18-02 team enforcement on real machines, run from the control host.

OWNER runs pb as the team owner on the machine that is shared (commands through OWNER_CMD, e.g.
`ssh pbvm2 sudo docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 lk-a`);
MEMBER runs pb as an accepted member of the team on another machine (MEMBER_CMD, e.g.
`ssh pujan@ubuntu24`). The check shares the owner machine into the team, then:
  no rule -> the member's exec is refused; a simple-view grant (pb team machine grant) -> exec
  works; a deny rule in the policy file -> exec is refused again and explain names the rule.
With the deny saved it also checks F18-04 (a failing in-file test blocks a save, owner guard,
who-can, what-can, explain on a draft), F18-05 (the review before save, audit events) and F18-08
(a member reads, explains only themselves, and cannot add a rule on another's machine) and F18-07
(pb team grant ... env is the member's env:team rule; explain follows grant and revoke).
Afterwards it restores the team policy's starting version and unshares the machine.

Usage: policy_team.py --team T --machine-id ID --machine-name NAME \
         --owner-cmd 'ssh pbvm2 sudo docker exec -i -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 lk-a' \
         --member-cmd 'ssh pujan@ubuntu24'
"""
import argparse
import json
import shlex
import subprocess
import sys
import time



def sh(prefix, pb, args, timeout=150, stdin=None):
    """Runs pb on one side: prefix is the ssh/docker command line, quoted once for its remote shell."""
    remote = ' '.join(shlex.quote(x) for x in ['timeout', '120', pb] + list(args))
    done = subprocess.run(shlex.split(prefix) + [remote], capture_output=True, text=True, timeout=timeout, input=stdin)
    return done.returncode, done.stdout, done.stderr


def data(out):
    d = json.loads(out)
    return d.get('data', d) if isinstance(d, dict) else d


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--team', required=True)
    p.add_argument('--machine-id', required=True)
    p.add_argument('--machine-name', required=True)
    p.add_argument('--owner-cmd', required=True)
    p.add_argument('--member-cmd', required=True)
    p.add_argument('--owner-pb', default='/home/pbt/.local/bin/pb')
    p.add_argument('--member-pb', default='.local/bin/pb')
    a = p.parse_args()
    owner = lambda *args, **kw: sh(a.owner_cmd, a.owner_pb, args, **kw)
    member = lambda *args, **kw: sh(a.member_cmd, a.member_pb, args, **kw)
    fails = []

    def step(name, ok, detail=''):
        print(('PASS ' if ok else 'FAIL ') + name + ('' if ok else ': ' + detail.strip()[-400:]))
        if not ok:
            fails.append(name)

    marker = 'f18-team-check'
    rc, out, err = owner('policy', 'get', '--workspace', a.team, '--json')
    start = data(out)['version'] if rc == 0 else None
    step('owner reads the team policy', rc == 0, err)
    if start is None:
        return 1
    def gen():
        rc, out, _ = owner('team', 'get', a.team, '--json')
        return str(data(out)['generation']) if rc == 0 else '0'

    rc, out, err = owner('team', 'machine', 'share', a.team, a.machine_id, '--generation', gen(), '--json')
    step('share the owner machine into the team', rc == 0, out + err)
    try:
        rc, out, err = member('exec', '--workspace', a.team, a.machine_name, '--', 'echo', marker)
        step('no rule: the member is refused', rc != 0 and marker not in out, out + err)
        rc, out, err = owner('team', 'machine', 'grant', a.team, a.machine_id, '--all-members', '--capability', 'exec',
                             '--generation', gen(), '--json')
        step('simple view: grant exec to every member', rc == 0, out + err)
        # A grant reaches both peer networks asynchronously: retry, bounded, until it has.
        for attempt in range(9):
            rc, out, err = member('exec', '--workspace', a.team, a.machine_name, '--', 'echo', marker)
            if rc == 0 and marker in out:
                break
            time.sleep(10)
        step('the member can exec', rc == 0 and marker in out, out + err)
        rc, out, err = owner('policy', 'get', '--workspace', a.team)
        try:
            doc = json.loads(out)
        except ValueError:
            doc = None
        step('the team policy is plain JSON to extend', doc is not None, out[:200])
        if doc is not None:
            deny_checks(a, owner, member, step, doc, marker)
            member_rights(a, owner, member, step, doc)
            env_checks(a, owner, step, gen)
    finally:
        rc, out, err = owner('policy', 'restore', str(start), '--workspace', a.team, '--message', marker + ' cleanup', '--yes')
        step('restore the starting policy version', rc == 0, out + err)
        rc, out, err = owner('team', 'activity', a.team, '--action', 'team.policy', '--since', '1h', '--json')
        actions = [e.get('action') for e in (data(out).get('items') or [])] if rc == 0 else []
        step('F18-05 saves and restores are audit events', 'team.policy.updated' in actions and 'team.policy.restored' in actions, out[-300:] + err)
        rc, out, err = owner('team', 'machine', 'unshare', a.team, a.machine_id, '--generation', gen(), '--json')
        step('unshare the machine', rc == 0, out + err)
    print('RESULT', 'PASS' if not fails else 'FAIL ' + ','.join(fails))
    return 1 if fails else 0


def put(prefix, path, text):
    """Writes text to path on one side (a draft for --file)."""
    subprocess.run(shlex.split(prefix) + ['sh -c ' + shlex.quote('cat > ' + shlex.quote(path))], input=text, text=True, timeout=60, check=True)


def code_in(out, err, code):
    return code in out or code in err


def deny_checks(a, owner, member, step, doc, marker):
    """F18-02 deny over allow, F18-04 explain/test and F18-05 review, with a deny rule saved."""
    granted = json.dumps(doc, indent=2) + '\n'
    target = 'machine:' + a.machine_name
    doc = json.loads(granted)
    doc.setdefault('rules', []).append({'src': ['*'], 'dst': [target], 'deny': ['exec'], 'note': marker})
    draft = json.dumps(doc, indent=2) + '\n'
    who = 'user:' + email(a.member_cmd, a.member_pb)
    rc, out, err = owner('policy', 'set', '-', '--workspace', a.team, '--dry-run', stdin=draft)
    step('F18-05 the review lists the member losing exec, nothing saved',
         rc == 0 and ('- lost ' + who + ' ' + target + ' exec') in out, out + err)
    failing = json.loads(draft)
    failing['tests'] = [{'src': who, 'dst': target, 'allow': ['exec']}]
    rc, out, err = owner('policy', 'set', '-', '--workspace', a.team, '--yes', stdin=json.dumps(failing))
    step('F18-04 a failing in-file test blocks the save', rc != 0 and '1 failed' in out, out + err)
    who_can = [x.get('subject') for x in explain(owner, a.team, '--dst', target, '--action', 'exec').get('who_can') or []]
    step('F18-04 who-can lists the member while the grant allows it', who in who_can, str(who_can))
    rc, out, err = owner('policy', 'set', '-', '--workspace', a.team, '--message', marker, '--yes', stdin=draft)
    step('a deny rule is saved after the access review', rc == 0, out + err)
    rc, out, err = member('exec', '--workspace', a.team, a.machine_name, '--', 'echo', marker)
    step('deny wins over the grant: the member is refused', rc != 0 and marker not in out, out + err)
    dec = explain(owner, a.team, '--src', who, '--dst', target, '--action', 'exec').get('decision') or {}
    step('explain names the deny rule', not dec.get('allowed') and dec.get('reason') == 'denied_by_rule' and len(dec.get('rules') or []) == 1, str(dec))
    who_can = [x.get('subject') for x in explain(owner, a.team, '--dst', target, '--action', 'exec').get('who_can') or []]
    step('F18-04 who-can no longer lists the member', who not in who_can, str(who_can))
    # F18-05: the file stores IDs, so a renamed machine reads with its new name and keeps its rules.
    renamed = a.machine_name + 'r'
    try:
        rc, out, err = owner('machine', 'rename', a.machine_id, renamed)
        rc2, text, _ = owner('policy', 'get', '--workspace', a.team)
        step('F18-05 a renamed machine reads with its new name in the policy',
             rc == 0 and rc2 == 0 and ('"machine:' + renamed + '"') in text and ('"' + target + '"') not in text, out + err + text[-200:])
    finally:
        owner('machine', 'rename', a.machine_id, a.machine_name)
    # The owner guard (PLAN F18 evaluation step 2) is the personal workspace's: a draft that denies
    # everything still leaves the owner every action on their own machine.
    me = 'user:' + email(a.owner_cmd, a.owner_pb)
    put(a.owner_cmd, '/tmp/f18-deny-all.hujson', json.dumps({'rules': [{'src': ['*'], 'dst': ['machine:*'], 'deny': ['*']}]}))
    dec = explain(owner, 'personal', '--src', me, '--dst', target, '--action', 'exec', '--file', '/tmp/f18-deny-all.hujson').get('decision') or {}
    step('F18-04 the owner guard holds against any deny (personal draft)', dec.get('allowed') is True and dec.get('reason') == 'owner', str(dec))
    what = explain(owner, a.team, '--src', who).get('what_can') or []
    step('F18-04 what-can has no exec on the machine for the member',
         not any(x.get('target') == target and x.get('action') == 'exec' for x in what), str(what)[:300])
    path = '/tmp/f18-draft.hujson'
    put(a.owner_cmd, path, granted)
    dec = explain(owner, a.team, '--src', who, '--dst', target, '--action', 'exec', '--file', path).get('decision') or {}
    step('F18-04 explain on a draft without the deny allows the member', dec.get('allowed') is True and dec.get('reason') == 'rule', str(dec))


def member_rights(a, owner, member, step, doc):
    """F18-08: a member reads the policy, explains only themselves and edits no rule on another's machine."""
    target = 'machine:' + a.machine_name
    rc, out, err = member('policy', 'get', '--workspace', a.team, '--json')
    can = data(out).get('can_edit') if rc == 0 else None
    step('F18-08 a member reads the policy and cannot edit everything', can in ('none', 'own_machines'), out[-200:] + err)
    doc = json.loads(json.dumps(doc))
    doc.setdefault('rules', []).append({'src': ['*'], 'dst': [target], 'allow': ['terminal']})
    rc, out, err = member('policy', 'set', '-', '--workspace', a.team, '--yes', stdin=json.dumps(doc))
    step("F18-08 a member cannot add a rule on another member's machine", rc != 0 and code_in(out, err, 'cannot save this policy change'), out[-300:] + err)
    who = 'user:' + email(a.member_cmd, a.member_pb)
    rc, out, err = member('policy', 'explain', '--workspace', a.team, '--src', who, '--dst', target, '--action', 'exec', '--json')
    step('F18-08 a member explains their own access', rc == 0, out[-200:] + err)
    me = 'user:' + email(a.owner_cmd, a.owner_pb)
    rc, out, err = member('policy', 'explain', '--workspace', a.team, '--src', me, '--dst', target, '--action', 'exec', '--json')
    step("F18-08 a member cannot explain someone else's access", rc != 0 and code_in(out, err, 'policy_explain_forbidden'), out[-300:] + err)


def env_checks(a, owner, step, gen):
    """F18-07: a team ENV grant is the member's policy rule; explain on env:team follows it."""
    rc, out, _ = sh(a.member_cmd, a.member_pb, ['auth', 'status', '--json'])
    d = data(out) if rc == 0 else {}
    account = (d.get('account') or {}).get('id') or d.get('account_id') or ''
    who = 'user:' + email(a.member_cmd, a.member_pb)
    rc, out, err = owner('team', 'grant', a.team, account, 'env', a.team, 'read', '--generation', gen(), '--json')
    step('F18-07 pb team grant gives the member the team ENV (team ENV must exist)', rc == 0, out[-300:] + err)
    if rc != 0:
        return
    try:
        rc, out, _ = owner('policy', 'get', '--workspace', a.team)
        rules = json.loads(out).get('rules') or []
        ruled = {'src': [who], 'dst': ['env:team'], 'allow': ['env_read']} in rules
        dec = explain(owner, a.team, '--src', who, '--dst', 'env:team', '--action', 'env_read').get('decision') or {}
        step('F18-07 the grant is the member\'s rule and explain allows env_read', ruled and dec.get('allowed') is True, str(dec))
        step('F18-07 explain says the member has no team key yet', dec.get('key_delivered') is False, str(dec))
    finally:
        rc, out, err = owner('team', 'grant', a.team, account, 'env', a.team, 'read', '--active=false', '--generation', gen(), '--json')
        step('F18-07 revoking the grant removes the rule', rc == 0, out[-300:] + err)
    dec = explain(owner, a.team, '--src', who, '--dst', 'env:team', '--action', 'env_read').get('decision') or {}
    step('F18-07 after the revoke explain denies env_read', dec.get('allowed') is False, str(dec))


def explain(run, team, *args):
    rc, out, _ = run('policy', 'explain', '--workspace', team, *args, '--json')
    try:
        return data(out) if rc == 0 else {}
    except ValueError:
        return {}


_emails = {}


def email(prefix, pb):
    if prefix not in _emails:
        rc, out, _ = sh(prefix, pb, ['auth', 'status', '--json'])
        d = data(out)
        _emails[prefix] = ((d.get('account') or {}).get('email') or d.get('email') or '').lower()
    return _emails[prefix]


if __name__ == '__main__':
    sys.exit(main())
