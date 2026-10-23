#!/usr/bin/env python3
"""F18-11 / C262 device check: machine tags from pb, run from the control host.

OWNER runs pb signed in on one machine of the account (OWNER_CMD, e.g.
`ssh pbvm3 sudo docker exec -i -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 lanek-c`);
TARGET is another machine of the account, by name. In the personal workspace the check adds,
removes and sets TARGET's tags with pb machine tag, checks pb machine list (TAGS, --tag), pb status
and the policy file, then clears the tags again.

Usage: machine_tags.py --owner-cmd CMD --target NAME [--pb PATH]
"""
import argparse
import json
import shlex
import subprocess
import sys
import time


def sh(prefix, pb, args, timeout=150):
    remote = ' '.join(shlex.quote(x) for x in ['timeout', '120', pb] + list(args))
    done = subprocess.run(shlex.split(prefix) + [remote], capture_output=True, text=True, timeout=timeout)
    return done.returncode, done.stdout, done.stderr


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--owner-cmd', required=True)
    p.add_argument('--target', required=True)
    p.add_argument('--pb', default='/home/pbt/.local/bin/pb')
    a = p.parse_args()
    pb = lambda *args: sh(a.owner_cmd, a.pb, args)
    fails = []

    def step(name, ok, detail=''):
        print(('PASS ' if ok else 'FAIL ') + name + ('' if ok else ': ' + detail.strip()[-400:]))
        if not ok:
            fails.append(name)

    def tags_json():
        rc, out, err = pb('machine', 'tag', a.target, '--workspace', 'personal', '--json')
        try:
            return json.loads(out).get('tags'), out + err
        except ValueError:
            return None, out + err

    try:
        rc, out, err = pb('machine', 'tag', a.target, 'add', 'smoke-ci', 'tag:smoke-prod', '--workspace', 'personal', '--yes')
        step('C262 add two tags (prefix optional)', rc == 0 and 'tag:smoke-ci, tag:smoke-prod' in out, out + err)
        rc, out, err = pb('machine', 'tag', a.target, 'remove', 'smoke-prod', '--workspace', 'personal', '--yes', '--json')
        try:
            ok = json.loads(out).get('tags') == ['tag:smoke-ci'] and json.loads(out).get('policy_version', 0) > 0
        except ValueError:
            ok = False
        step('C262 remove one tag (--json names the new policy version)', rc == 0 and ok, out + err)
        rc, out, err = pb('machine', 'tag', a.target, 'set', 'smoke-web', 'smoke-ci', '--workspace', 'personal', '--yes')
        step('C262 set the full list', rc == 0 and 'tag:smoke-ci, tag:smoke-web' in out, out + err)
        got, detail = tags_json()
        step('C262 pb machine tag <machine> shows the tags', got == ['tag:smoke-ci', 'tag:smoke-web'], detail)
        rc, out, err = pb('machine', 'tag', a.target, 'add', 'Bad Tag', '--workspace', 'personal')
        step('C262 an invalid tag is refused', rc != 0 and 'tag:<name>' in (out + err), out + err)
        rc, out, err = pb('machine', 'list', '--workspace', 'personal', '--tag', 'smoke-web')
        rows = [l for l in out.splitlines() if l and not l.startswith('NAME')]
        step('F18-11 pb machine list --tag lists only the tagged machine, with TAGS',
             rc == 0 and 'TAGS' in out and len(rows) == 1 and rows[0].split()[0] == a.target and 'tag:smoke-web' in rows[0], out + err)
        rc, out, err = pb('policy', 'get', '--workspace', 'personal')
        try:
            tags = json.loads(out).get('tags') or {}
            ok = tags.get('tag:smoke-web') == ['machine:' + a.target] and 'tag:smoke-prod' not in tags
        except ValueError:
            ok = False
        step('F18-11 the tags live in the policy file (an emptied tag is removed)', rc == 0 and ok, out[-300:] + err)
        # pb status reads the daemon's snapshot, refreshed from the server's inventory.
        ok, out = False, ''
        for _ in range(12):
            rc, out, err = pb('status')
            block = out.split('\n' + a.target + ' (')
            if len(block) > 1 and 'Tags: tag:smoke-ci, tag:smoke-web' in block[1].split('\n\n')[0]:
                ok = True
                break
            time.sleep(10)
        step('F18-11 pb status shows the machine tags', ok, out[-400:])
    finally:
        rc, out, err = pb('machine', 'tag', a.target, 'set', '--workspace', 'personal', '--yes')
        step('clear the tags again', rc == 0 and 'no tags' in out, out + err)
    print('RESULT', 'PASS' if not fails else 'FAIL ' + ','.join(fails))
    return 1 if fails else 0


if __name__ == '__main__':
    sys.exit(main())
