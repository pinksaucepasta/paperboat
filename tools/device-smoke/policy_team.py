#!/usr/bin/env python3
"""F18-02 team enforcement on real machines, run from the control host.

OWNER runs pb as the team owner on the machine that is shared (commands through OWNER_CMD, e.g.
`ssh pbvm2 sudo docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 lk-a`);
MEMBER runs pb as an accepted member of the team on another machine (MEMBER_CMD, e.g.
`ssh pujan@ubuntu24`). The check shares the owner machine into the team, then:
  no rule -> the member's exec is refused; a simple-view grant (pb team machine grant) -> exec
  works; a deny rule in the policy file -> exec is refused again and explain names the rule.
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
        text = out
        try:
            doc = json.loads(text)
        except ValueError:
            doc = None
        step('the team policy is plain JSON to extend', doc is not None, text[:200])
        if doc is not None:
            doc.setdefault('rules', []).append({'src': ['*'], 'dst': ['machine:' + a.machine_name], 'deny': ['exec'], 'note': marker})
            draft = json.dumps(doc, indent=2) + '\n'
            rc, out, err = owner('policy', 'set', '-', '--workspace', a.team, '--message', marker, '--yes', stdin=draft)
            step('a deny rule is saved after the access review', rc == 0, out + err)
            rc, out, err = member('exec', '--workspace', a.team, a.machine_name, '--', 'echo', marker)
            step('deny wins over the grant: the member is refused', rc != 0 and marker not in out, out + err)
            rc, out, err = owner('policy', 'explain', '--workspace', a.team, '--src', 'user:' + memberEmail(a), '--dst', 'machine:' + a.machine_name, '--action', 'exec', '--json')
            ok = False
            try:
                dec = data(out)['decision']
                ok = (not dec['allowed']) and dec['reason'] == 'denied_by_rule' and len(dec.get('rules', [])) == 1
            except (ValueError, KeyError, TypeError):
                pass
            step('explain names the deny rule', ok, out + err)
    finally:
        rc, out, err = owner('policy', 'restore', str(start), '--workspace', a.team, '--message', marker + ' cleanup', '--yes')
        step('restore the starting policy version', rc == 0, out + err)
        rc, out, err = owner('team', 'machine', 'unshare', a.team, a.machine_id, '--generation', gen(), '--json')
        step('unshare the machine', rc == 0, out + err)
    print('RESULT', 'PASS' if not fails else 'FAIL ' + ','.join(fails))
    return 1 if fails else 0


_member_email = None


def memberEmail(a):
    global _member_email
    if _member_email is None:
        rc, out, _ = sh(a.member_cmd, a.member_pb, ['auth', 'status', '--json'])
        d = data(out)
        _member_email = (d.get('account') or {}).get('email') or d.get('email') or ''
    return _member_email


if __name__ == '__main__':
    sys.exit(main())
