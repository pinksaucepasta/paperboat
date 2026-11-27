#!/usr/bin/env python3
"""Fixture preflight: every operator fixture a smoke check needs exists and works, before any shard starts.

usage: preflight_fixtures.py [--windows] [--checks ID,ID,...] [--only-ids '|a|b|'] [--skip a,b] [--list]
Run by smoke.sh (before its shards) and windows_checks.py (before its checks) with the check ids they are about to run;
with no --checks/--only-ids every check that needs a fixture is verified. Each missing item prints what is missing and how
to fix it, all of them in one report, and the exit code is 2. Nothing is created here (the per-run machines are made by
the checks themselves). Settings come from the `smoke` object of ~/.config/paperboat-dev/targets.json (README.md, "Fixtures").
Secrets (console sessions, S3 keys) are only checked for existence, mode and a 200 from /v1/me; never printed.
"""
import argparse
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import sys

import ct_env
import dev_targets
import recipe_checks

HERE = Path(__file__).resolve().parent
ENV = dev_targets.smoke_environment(dev_targets.load())

# check id -> the fixtures it needs (names of the verifiers below)
NEEDS = {
    'config-team-default': ['operator_session', 'config_repo', 'team_admin', 'team_plan', 'fixture_host'],
    'free-plan': ['free_session', 'free_plan', 'fixture_host'],
    'team-business': ['operator_session', 'team_members', 'team_plan', 's3_keys', 'fixture_host'],
    'ssh-rsync': ['windows_rsync'],
    'recording-key-rotation': ['console_tools', 'fixture_host'],
    'console-team-admin': ['console_tools', 'operator_session', 'team_admin'],
    'console-signout-switch': ['console_tools', 'operator_session', 'operator_password', 'team_admin'],
    'console-invite-mail': ['console_tools', 'operator_session', 'team_admin'],
    'console-recording-replay': ['console_tools', 'fixture_host'],
    # recipe_checks.py: throwaway console accounts and containers; the clean real machines are optional (their rows are NOT RUN without)
    **{check: ['console_tools', 'fixture_host'] for check in recipe_checks.CHECKS},
}
# the same checks run from windows_checks.py (the device is the machine): no Docker host, no Free fixture
NEEDS_WINDOWS = {'ssh-rsync': ['windows_rsync'], 'config-team-default': ['operator_session', 'config_repo', 'team_admin', 'team_plan'],
                 'team-business': ['operator_session', 'team_members', 'team_plan']}
FIX_ACCOUNTS = 'tools/device-smoke/README.md "Fixtures"'


def setting(name, default=''):
    return os.path.expanduser(ENV.get(name, default))


def console(state_key, method, path):
    """(status, body) of one console API call as the account of the stored session; (0, reason) when it cannot run."""
    state = setting(state_key)
    needed = {'SMOKE_CONSOLE_ORIGIN': setting('SMOKE_CONSOLE_ORIGIN'), 'SMOKE_PW_CHROMIUM': setting('SMOKE_PW_CHROMIUM')}
    missing = [k for k, v in needed.items() if not v]
    if missing or not state or not Path(state).is_file():
        return 0, 'set %s in targets.json smoke and create the session file %s' % (', '.join(missing) or state_key, state or '(%s unset)' % state_key)
    env = dict(os.environ, ORIGIN=needed['SMOKE_CONSOLE_ORIGIN'], STATE=state, PW_CHROMIUM=needed['SMOKE_PW_CHROMIUM'])
    if setting('SMOKE_PW_MODULE'):
        env['PW_MODULE'] = setting('SMOKE_PW_MODULE')
    try:
        done = subprocess.run(['node', str(HERE / 'console_api.mjs'), method, path], env=env, capture_output=True, text=True, timeout=120)
        reply = json.loads(done.stdout.strip().splitlines()[-1])
    except (OSError, ValueError, IndexError, subprocess.TimeoutExpired) as error:
        return 0, 'console_api.mjs failed: %s' % (type(error).__name__,)
    return reply['status'], reply['body']


def session(state_key, who, fix):
    status, body = console(state_key, 'GET', '/v1/me')
    if status == 200:
        return None
    return '%s console session is not valid (%s). Fix: %s' % (who, body if status == 0 else 'HTTP %s' % status, fix)


def operator_session():
    return session('SMOKE_OPERATOR_STATE', 'operator (smoke team owner)',
                   'node console_session.mjs password pprbtdev@gmail.com ~/.config/paperboat/console-workos.password $SMOKE_OPERATOR_STATE '
                   '(env ORIGIN=$SMOKE_CONSOLE_ORIGIN PW_CHROMIUM PW_MODULE)')


def free_session():
    return session('SMOKE_FREE_STATE', 'Free fixture account', 'sign in again with the credentials in $SMOKE_FREE_ACCOUNT_DIR/account.json, or create a new Free account with fresh_account.mjs signup (' + FIX_ACCOUNTS + ')')


def free_plan():
    status, body = console('SMOKE_FREE_STATE', 'GET', '/v1/billing/workspaces/personal')
    data = body.get('data') if isinstance(body, dict) else None
    if status == 200 and data and data.get('state') == 'free' and data.get('seats_bought') == 0:
        return None
    return 'the Free fixture account is not a plain Free account (%s). Fix: the account must never subscribe; if it did, create a new one (%s)' % (
        'HTTP %s' % status if status != 200 else 'state=%s seats=%s' % (data.get('state'), data.get('seats_bought')), FIX_ACCOUNTS)


def config_repo():
    repo = setting('SMOKE_CONFIG_REPO')
    if not repo:
        return 'SMOKE_CONFIG_REPO is not set. Fix: connect a private GitHub config repository to the smoke team owner and set it (%s)' % FIX_ACCOUNTS
    status, body = console('SMOKE_OPERATOR_STATE', 'GET', '/v1/config-repositories')
    items = body.get('data', {}).get('items', []) if status == 200 and isinstance(body, dict) else []
    if not any(i.get('provider') == 'github' and i.get('display_name') == repo and i.get('state') == 'active' for i in items):
        return 'the config repository %s is not connected as an active GitHub repository of the operator account. Fix: console Config > Add repository, or POST /v1/config-repositories (%s)' % (repo, FIX_ACCOUNTS)
    return None


def team_json():
    team = setting('PAPERBOAT_SMOKE_TEAM', 'paperboat-smoke-paid')
    status, body = console('SMOKE_OPERATOR_STATE', 'GET', '/v1/teams/' + team)
    return team, (body.get('data') if status == 200 and isinstance(body, dict) else None)


def team_admin():
    team, data = team_json()
    if not data or data.get('my_role') not in ('owner', 'admin'):
        return 'the operator account does not administer team %s. Fix: use the retained paid smoke team (%s)' % (team, FIX_ACCOUNTS)
    return None


def team_members():
    team, data = team_json()
    if not data or data.get('my_role') != 'owner' or data.get('member_count', 0) < 2:
        return 'team %s must be owned by the operator account and have another member (F19 role changes). Fix: %s' % (team, FIX_ACCOUNTS)
    return None


def team_plan():
    cred = Path.home() / '.config/paperboat-dev/operator-cred'
    if not cred.is_file():
        return 'the operator credential file ~/.config/paperboat-dev/operator-cred (email, password, authenticator key; mode 600) is missing (%s)' % FIX_ACCOUNTS
    done = subprocess.run([sys.executable, str(HERE / 'operator_fixture.py'), 'status'], capture_output=True, text=True, timeout=120)
    if done.returncode:
        return 'the smoke team has no running paid plan (30+ days) with the SSO and SCIM add-ons (%s). Fix: python3 tools/device-smoke/operator_fixture.py ensure' % (done.stdout.strip() or done.stderr.strip())[-160:]
    return None


def operator_password():
    path = Path(setting('SMOKE_OPERATOR_PASSWORD_FILE', '~/.config/paperboat-dev/console-workos.password'))
    if not path.is_file() or path.stat().st_mode & 0o077:
        return 'the smoke team owner password file %s is missing or not mode 0600 (set SMOKE_OPERATOR_PASSWORD_FILE; %s)' % (path, FIX_ACCOUNTS)
    return None


def s3_keys():
    for name in ('.f19-s3ak', '.f19-s3sk'):
        path = Path.home() / name
        if not path.is_file() or path.stat().st_mode & 0o077:
            return 'the S3 fixture key ~/%s is missing or not mode 0600. Fix: %s' % (name, FIX_ACCOUNTS)
    return None


def ssh(host, command, timeout=60):
    return subprocess.run(['ssh', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=15', host, command], capture_output=True, text=True, timeout=timeout, stdin=subprocess.DEVNULL)


def fixture_host():
    host, docker = setting('SMOKE_FIXTURE_HOST'), ct_env.CT_DOCKER
    pb = setting('SMOKE_FIXTURE_PB')
    if not host:
        return 'SMOKE_FIXTURE_HOST (an ssh host with Docker for the per-run fixture machines, e.g. a linuxpool runner) is not set (%s)' % FIX_ACCOUNTS
    try:
        done = ssh(host, '%s info --format {{.ServerVersion}} && uname -m' % docker)
    except subprocess.TimeoutExpired:
        done = None
    if done is None or done.returncode:
        return 'fixture host %s does not answer ssh/docker (SMOKE_CT_DOCKER=%s). Fix: start or adopt a linuxpool runner and set SMOKE_FIXTURE_HOST' % (host, docker)
    if not pb or not Path(pb).is_file():
        return 'SMOKE_FIXTURE_PB (the Linux build for %s, arch %s) is not set or missing' % (host, done.stdout.split()[-1])
    return None


def windows_rsync():
    login = setting('SMOKE_WINDOWS_SSH')
    password = setting('SMOKE_WINDOWS_PASSWORD')
    if not login or not password or not shutil.which('sshpass'):
        return 'SMOKE_WINDOWS_SSH, SMOKE_WINDOWS_PASSWORD and sshpass are needed to look at the Windows target'
    script = 'rsync --version | Select -First 1'
    import base64
    encoded = base64.b64encode(script.encode('utf-16-le')).decode()
    done = subprocess.run(['sshpass', '-e', 'ssh', '-o', 'StrictHostKeyChecking=accept-new', '-o', 'ConnectTimeout=15', login,
                           'powershell -NoProfile -NonInteractive -EncodedCommand ' + encoded],
                          capture_output=True, text=True, timeout=90, env=dict(os.environ, SSHPASS=password), stdin=subprocess.DEVNULL)
    if 'rsync' not in done.stdout.lower() or 'version' not in done.stdout.lower():
        return 'rsync is not on the PATH of %s. Fix (official MSYS2): C:\\msys64\\usr\\bin\\pacman -S --noconfirm rsync, and C:\\msys64\\usr\\bin on the machine PATH' % login
    return None


def console_tools():
    missing = [t for t in ('node',) if not shutil.which(t)] + [p for p in (setting('SMOKE_PW_CHROMIUM'), setting('SMOKE_PW_MODULE')) if not p or not Path(p).exists()]
    return ('console automation is missing: %s. Fix: %s' % (', '.join(missing) or 'SMOKE_PW_CHROMIUM / SMOKE_PW_MODULE unset', FIX_ACCOUNTS)) if missing or not setting('SMOKE_PW_CHROMIUM') else None


VERIFIERS = {f.__name__: f for f in (team_plan, operator_session, free_session, free_plan, config_repo, team_admin, team_members,
                                      s3_keys, fixture_host, windows_rsync, console_tools, operator_password)}


def selected(args):
    ids = [i for i in args.checks.split(',') if i] if args.checks else list(NEEDS_WINDOWS if args.windows else NEEDS)
    if args.only_ids.strip('|'):
        only = set(args.only_ids.strip('|').split('|')) | {i.split(':')[0] for i in args.only_ids.strip('|').split('|')}
        ids = [i for i in ids if i in only]
    skip = {i for i in args.skip.split(',') if i}
    return [i for i in ids if i in (NEEDS_WINDOWS if args.windows else NEEDS) and i not in skip]


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.split('\n')[0])
    parser.add_argument('--checks', default='')
    parser.add_argument('--only-ids', default='')
    parser.add_argument('--skip', default='')
    parser.add_argument('--windows', action='store_true', help='the checks as windows_checks.py runs them')
    parser.add_argument('--list', action='store_true', help='print the checks and the fixtures they need, verify nothing')
    args = parser.parse_args(argv)
    ids = selected(args)
    needs = NEEDS_WINDOWS if args.windows else NEEDS
    if args.list:
        for i in ids:
            print(i, '->', ', '.join(needs[i]))
        return 0
    problems, done = [], {}
    for check in ids:
        for name in needs[check]:
            if name not in done:
                try:
                    done[name] = VERIFIERS[name]()
                except Exception as error:  # noqa: BLE001 a verifier that cannot run is a missing fixture, named
                    done[name] = '%s could not be verified: %s: %s' % (name, type(error).__name__, error)
            if done[name]:
                problems.append((check, name, done[name]))
    for check in ids:
        bad = [n for c, n, _ in problems if c == check]
        print('fixtures %-24s %s' % (check, 'MISSING ' + ', '.join(bad) if bad else 'ok'), file=sys.stderr)
    for check, name, text in problems:
        print('  - %s / %s: %s' % (check, name, text), file=sys.stderr)
    if problems:
        print('fixture preflight FAILED: nothing was started. Fix the items above (%s).' % FIX_ACCOUNTS, file=sys.stderr)
        return 2
    return 0


if __name__ == '__main__':
    sys.exit(main())
