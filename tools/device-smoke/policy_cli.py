#!/usr/bin/env python3
"""F18 device check (PLAN F18-10, CHECKLIST C244-C253) on the machine itself: the steps of policy_cli.sh without a shell.

  policy_cli.py TARGET

pb policy get/set/test/explain/versions/restore/schema and pb api-token create/list/revoke against the live server, in
the personal workspace of the signed-in account; TARGET is a machine name of that account. policy_cli.sh needs bash and
python3 on the machine, which the Windows device does not have: this runs the same 13 steps with pb alone (pb.exe on the
device when PB_SMOKE_WINDOWS_PB is set, see winhost.py). Prints PASS/FAIL per step and RESULT; never prints secrets. The
personal policy is restored to the version it had before the run, and the token is revoked, also when a step fails.
"""
import argparse
import json
import re
import sys

import winhost

PB = winhost.pb_path()
WORKSPACE = ['--workspace', 'personal']


def pb(*arguments):
    done = winhost.pb_text(PB, arguments, timeout=90)
    return done.returncode, done.stdout, done.stdout + done.stderr


def parsed(out):
    value = json.loads(out)
    return value.get('data', value) if isinstance(value, dict) else value


def policy_file(email, target):
    return ('{\n  // F18 device check: tag the throwaway machine %s\n  "tags": {"tag:smoke-ci": ["machine:%s"]},\n  "rules": [],\n'
            '  "tests": [{"src": "user:%s", "dst": "machine:%s", "allow": ["exec", "terminal"]}],\n}\n' % (target, target, email, target))


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('target')
    target = parser.parse_args().target
    fails = []

    def step(name, check, text):
        """check() may raise on a reply of the wrong shape: that is a FAIL with the reply's last lines."""
        try:
            ok = bool(check())
        except (ValueError, KeyError, TypeError, IndexError):
            ok = False
        print(('PASS ' if ok else 'FAIL ') + name + ('' if ok else ': ' + re.sub(r'pbat_[A-Za-z0-9_-]*', 'pbat_REDACTED', text).strip()[-300:]))
        if not ok:
            fails.append(name)
        return ok

    email = parsed(pb('auth', 'status', '--json')[1])['account']['email']
    rc, out, text = pb('policy', 'schema')
    step('C250-schema', lambda: json.loads(out)['title'] == 'Paperboat workspace policy', text)
    rc, out, text = pb('policy', 'get', *WORKSPACE, '--json')
    if not step('C244-get', lambda: 'can_edit' in parsed(out) and parsed(out)['version'] is not None, text):
        print('RESULT FAIL ' + ','.join(fails))
        return 1
    version = str(parsed(out)['version'])
    rc, out, text = pb('policy', 'versions', *WORKSPACE, '--json')
    step('C248-versions', lambda: len(parsed(out)['versions']) >= 1, text)
    saved = created = False
    with winhost.workdir('pb-policy-') as directory:
        good, bad = directory / 'lkpol.hujson', directory / 'lkbad.json'
        good.write_text(policy_file(email, target))
        bad.write_text('{"rules": [{"src": ["*"], "dst": ["machine:nope"], "allow": ["exec"]}]}')
        try:
            rc, out, text = pb('policy', 'test', *WORKSPACE, str(good))
            step('C246-test', lambda: rc == 0, text)
            rc, out, text = pb('policy', 'test', *WORKSPACE, str(bad))
            step('C246-test-invalid', lambda: rc != 0 and '/rules/0/dst/0: unknown_machine' in text, text)
            rc, out, text = pb('policy', 'set', *WORKSPACE, str(good), '--message', 'F18 device check', '--yes')
            saved = step('C245-set', lambda: rc == 0, text)
            rc, out, text = pb('policy', 'get', *WORKSPACE)
            step('C244-get-file-with-comments', lambda: 'tag:smoke-ci' in text and '// F18 device check' in text, text)
            rc, out, text = pb('policy', 'explain', *WORKSPACE, '--src', 'user:' + email, '--dst', 'machine:' + target,
                               '--action', 'exec', '--json')
            step('C247-explain-owner', lambda: parsed(out)['decision']['allowed'] and parsed(out)['decision']['reason'] == 'owner', text)
        finally:
            if saved:
                rc, out, text = pb('policy', 'restore', *WORKSPACE, version, '--yes')
                step('C249-restore v' + version, lambda: rc == 0, text)
        if saved:
            rc, out, text = pb('policy', 'versions', *WORKSPACE, '--json')
            step('C248-versions-kinds', lambda: [item['kind'] for item in parsed(out)['versions']][:2] == ['restored', 'saved'], text)
    scope = json.dumps([{'dst': ['machine:' + target], 'allow': ['exec']}])
    try:
        rc, out, text = pb('api-token', 'create', 'smoke-ci', *WORKSPACE, '--expires', '1d', '--scope', scope, '--json')
        created = rc == 0
        step('C251-create', lambda: parsed(out)['secret'].startswith('pbat_') and parsed(out)['api_token']['name'] == 'smoke-ci', text)
        rc, out, text = pb('api-token', 'list', *WORKSPACE)
        step('C252-list', lambda: 'smoke-ci' in text and 'pbat_' not in text, text)
    finally:
        if created:
            rc, out, text = pb('api-token', 'revoke', 'smoke-ci', *WORKSPACE, '--yes')
            step('C253-revoke', lambda: rc == 0 and 'Revoked API token smoke-ci' in text, text)
    print('RESULT', 'PASS' if not fails else 'FAIL ' + ','.join(fails))
    return 1 if fails else 0


if __name__ == '__main__':
    sys.exit(main())
