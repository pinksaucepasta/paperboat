#!/usr/bin/env python3
"""F23-01 / F5-10 on a Mac with TWO OS users, each paired to its OWN Paperboat account: managed SSH works for both at once,
sessions stay isolated per user, and revoking one user's machine leaves the other's untouched.

Usage: ssh_mac_two_users.py  (env below; wrappers take ONE shell-string argument, as in ssh_mac_rows.py)
  A_CMD / B_CMD   run it on a Linux client signed in as account A / account B (containers, e.g. macl-c / macl-r)
  MAC_CMD         runs it on the Mac
  A_ALIAS, A_USER  account A's machine alias on the Mac and its OS user (default apple / admin)
  B_ALIAS, B_USER  account B's machine alias and OS user (default apple-b / pbmac2)
Steps: both clients hold a `pb ssh` session at the same time; each lands as its own OS user; the Mac runs one sshd session
per user, owned by that user; neither account can reach the other's machine; account B revokes its machine: B's session ends
within 15 s and B is refused, A's session is untouched and a new A session works.
Prints one JSON line; exit 1 on FAIL. The B machine stays revoked (re-pair pbmac2 to run again).
"""
import json
import os
import re
import sys
import time

import ssh_mac_rows as m

A_ALIAS, A_USER = os.environ.get('A_ALIAS', 'apple'), os.environ.get('A_USER', 'admin')
B_ALIAS, B_USER = os.environ.get('B_ALIAS', 'apple-b'), os.environ.get('B_USER', 'pbmac2')
PB = '~/.local/bin/pb'
REVOKE_MAX = 15.0


def hold(env, tag, alias, user):
    script = ('rm -f /tmp/two-%s.out; nohup %s ssh %s -- "echo WHO=\\$(id -un); sleep 240" > /tmp/two-%s.out 2>&1 < /dev/null & echo $! > /tmp/two-%s.pid' %
              (tag, PB, alias, tag, tag))
    return m.run(env, script, 30)


def alive(env, tag):
    rc, out = m.run(env, 'kill -0 $(cat /tmp/two-%s.pid) 2>/dev/null && echo up || echo down' % tag, 30)
    return out.strip().endswith('up')


def landed_as(env, tag):
    return re.sub(r'\s+', '', m.run(env, 'grep -o "WHO=[a-z0-9_]*" /tmp/two-%s.out | head -1' % tag, 30)[1])


def stop(env, tag):
    m.run(env, 'p=$(cat /tmp/two-%s.pid 2>/dev/null); [ -n "$p" ] && { pkill -P $p; kill $p; }; true' % tag, 30)


def refused(env, alias):
    rc, out = m.run(env, '%s ssh %s -- id -un 2>&1' % (PB, alias), 90)
    return rc != 0 and 'WHO' not in out, out.strip().splitlines()[-1][:160] if out.strip() else ''


def machine_id(env, alias):
    rc, out = m.run(env, '%s --workspace personal machine list --json' % PB, 60)
    data = json.loads(out[out.index('{'):])
    return next(x['id'] for x in data['machines'] if x['alias'] == alias)


def revoke(env, mid):
    rc, out = m.run(env, '%s machine revoke %s --json 2>&1' % (PB, mid), 60)
    token = json.loads(out[out.index('{'):])['data']['confirmation_token']
    rc, out = m.run(env, '%s machine revoke %s --json --confirm %s 2>&1' % (PB, mid, token), 60)
    return '"confirmed"' in out


def main():
    detail = {}
    try:
        b_id = machine_id('B_CMD', B_ALIAS)
        hold('A_CMD', 'A', A_ALIAS, A_USER)
        hold('B_CMD', 'B', B_ALIAS, B_USER)
        deadline = time.time() + 40
        while time.time() < deadline and not (landed_as('A_CMD', 'A') and landed_as('B_CMD', 'B')):
            time.sleep(2)
        detail['landed'] = {'A': landed_as('A_CMD', 'A'), 'B': landed_as('B_CMD', 'B')}
        assert detail['landed'] == {'A': 'WHO=' + A_USER, 'B': 'WHO=' + B_USER}, 'sessions did not land as their own OS users: %s' % detail['landed']
        assert alive('A_CMD', 'A') and alive('B_CMD', 'B'), 'both sessions must be open at the same time'
        ps = m.mac('ps -axo user=,args= | grep -E "sshd-session|sshd:" | grep -v grep | grep -v "\\[priv\\]" | awk \'{print $1}\' | sort | uniq -c')
        users = {line.split()[1]: int(line.split()[0]) for line in ps.splitlines() if len(line.split()) == 2}
        detail['mac_sshd_sessions_by_user'] = users
        assert users.get(A_USER, 0) >= 1 and users.get(B_USER, 0) >= 1, 'the Mac must run one sshd session per user, owned by that user: %s' % users
        for env, alias, name in (('A_CMD', B_ALIAS, 'A->B'), ('B_CMD', A_ALIAS, 'B->A')):
            ok, line = refused(env, alias)
            detail['cross_' + name] = line
            assert ok, 'cross-account access was not refused (%s): %s' % (name, line)
        t0 = time.time()
        assert revoke('B_CMD', b_id), 'revoke of the B machine was not confirmed'
        while time.time() - t0 < 60 and alive('B_CMD', 'B'):
            time.sleep(1)
        detail['b_session_ended_after_s'] = round(time.time() - t0, 1)
        assert detail['b_session_ended_after_s'] <= REVOKE_MAX + 5, 'B session outlived the revoke by %.1f s' % detail['b_session_ended_after_s']
        assert alive('A_CMD', 'A'), "A's held session ended when B was revoked"
        rc, out = m.run('A_CMD', '%s ssh %s -- echo A-after-revoke 2>&1' % (PB, A_ALIAS), 90)
        assert 'A-after-revoke' in out, "a new A session failed after B's revoke: " + out[-160:]
        ok, line = refused('B_CMD', B_ALIAS)
        detail['b_after_revoke'] = line
        assert ok, 'B was not refused after its machine was revoked'
        print(json.dumps({'check': 'ssh-mac-two-users', 'status': 'PASS', **detail}))
        return 0
    except Exception as exc:
        print(json.dumps({'check': 'ssh-mac-two-users', 'status': 'FAIL', 'error': str(exc)[:400], **detail}))
        return 1
    finally:
        stop('A_CMD', 'A')
        stop('B_CMD', 'B')


if __name__ == '__main__':
    sys.exit(main())
