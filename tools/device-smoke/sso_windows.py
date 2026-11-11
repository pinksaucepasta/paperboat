#!/usr/bin/env python3
"""F19-03 (SSO sign-in) and F19-04 (Require SSO) from the Windows `pb` CLI, over ssh to one Windows runner.

Every result line names the device, the command that ran and its captured exit code. The device is
SMOKE_WINDOWS_SSH (e.g. winpool-9) with pb.exe at --pb (PB_SMOKE_WINDOWS_PB); the controller drives the console
sessions (console_session.mjs states) that approve each `pb login`, and mints the owner's API token.

  login        pb login --no-browser --json on the device's default profile, approved by the SSO user's
               console session (--sso-state); the exit code is read from the process itself (F19-03).
  require-sso  the device's default profile is a real, non-SSO team member signed in with `pb login`
               (--member-state approves it). An owner profile (token session of --owner-state, isolated) turns
               Require SSO on and off; the member's `pb team get` is refused with team_sso_required, works again
               when the member is exempt and when Require SSO is off (F19-04).

Usage: sso_windows.py login|require-sso --team T [--sso-state F | --member-state F --owner-state F] --out FILE
env: ORIGIN, PW_CHROMIUM, [PW_MODULE], SMOKE_WINDOWS_SSH, PB_SMOKE_WINDOWS_PB
"""
import argparse
import json
import os
import re
import subprocess
import sys
import time
from pathlib import Path

import winhost
from teammate_profile import environment, prepare, profile_root

HERE = Path(__file__).resolve().parent


class Log:
    def __init__(self, path):
        self.path, self.fails = path, []

    def step(self, name, command, rc, ok, code=None, detail=''):
        line = {'t': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()), 'step': name, 'device': os.environ['SMOKE_WINDOWS_SSH'],
                'command': command, 'rc': rc, 'code': code, 'result': 'pass' if ok else 'FAIL', 'detail': detail}
        if not ok:
            self.fails.append(name)
        with open(self.path, 'a') as out:
            out.write(json.dumps(line) + '\n')
        print(json.dumps(line), flush=True)

    def verdict(self, **extra):
        line = {'verdict': 'FAIL' if self.fails else 'PASS', 'fails': self.fails, **extra}
        with open(self.path, 'a') as out:
            out.write(json.dumps(line) + '\n')
        print(json.dumps(line), flush=True)
        return 1 if self.fails else 0


def mint_token(state, name):
    """A one-day personal API token of the account in the console session state (never printed)."""
    done = subprocess.run(['node', str(HERE / 'mint_api_token.mjs'), name], capture_output=True, text=True, timeout=240,
                          env=dict(os.environ, STATE=state))
    if done.returncode:
        raise RuntimeError('mint_api_token failed: ' + done.stderr.strip()[-200:])
    return done.stdout


def profile(pb, name):
    """An isolated profile (XDG overlay) next to the device's own: (config path, environment)."""
    root = winhost.device().home() / '.pb-smoke' / name
    config = root / 'config.json'
    if not config.exists():
        shown = json.loads(pb.run_remote(['config', 'show', '--json'], timeout=40).stdout)
        prepare(root, {'server_url': shown['server_url'], 'auth': {'allow_file_fallback': bool(shown.get('auth_file_fallback'))}})
    return str(config), environment(root)


def run(pb, log, name, arguments, env=None, input=None, expect=0, code=None):
    """Run pb on the device, log device + command + exit code; ok when the exit code is expect (and code is shown)."""
    config = ['--config', arguments[0]] if env else []
    argv = (config + arguments[1:]) if env else arguments
    done = pb.run_remote(argv, env=env, input=input if input is not None else '', timeout=120)
    shown = re.search(r'"code"\s*:\s*"([a-z_]+)"', done.stdout + done.stderr)
    found = shown.group(1) if shown else None
    ok = done.returncode == expect and (code is None or found == code)
    log.step(name, 'pb ' + ' '.join(a for a in argv if a != '-')[:200], done.returncode, ok, found, done.stdout.strip()[-160:])
    return done


def login(pb, log, state, name):
    """pb login --no-browser --json on the default profile, approved with the console session in state."""
    arguments = ['login', '--no-browser', '--json']
    with pb.device.temp_dir('sso-login') as directory:
        process = pb.device.spawn([str(pb), *arguments], directory)
        deadline = time.monotonic() + 60
        while b'\n' not in process.stdout():
            if time.monotonic() > deadline or process.returncode() is not None:
                raise RuntimeError('login printed no approval link within 60 s')
            time.sleep(1)
        receipt = json.loads(process.stdout().split(b'\n', 1)[0])
        link = receipt.get('data', receipt)['approval_url']
        approved = subprocess.run(['node', str(HERE / 'approve_cli_login.mjs'), link], capture_output=True, text=True, timeout=180,
                                  env=dict(os.environ, STATE=state))
        rc = process.wait(600)
        tail = process.stdout().decode(errors='replace').strip().splitlines()[-1:]
        process.kill()
    log.step(name, 'pb ' + ' '.join(arguments), rc, rc == 0 and approved.returncode == 0, None,
             'approved=%s; %s' % (approved.stdout.strip() or approved.stderr.strip()[-80:], (tail or [''])[0][-120:]))
    return rc


def whoami(pb):
    return json.loads(pb.run_remote(['auth', 'status', '--json'], timeout=40).stdout)


def sso_login(pb, log, args):
    login(pb, log, args.sso_state, 'sso_login_exit_code')
    status = whoami(pb)
    log.step('auth_status_signed_in', 'pb auth status --json', 0, bool(status.get('signed_in')), None,
             'email=%s' % (status.get('email') or (status.get('account') or {}).get('email')))
    run(pb, log, 'team_get_as_sso_user', ['team', 'get', args.team, '--json'])


def require_sso(pb, log, args):
    login(pb, log, args.member_state, 'member_login_exit_code')
    member = (whoami(pb).get('account') or {}).get('id') or whoami(pb).get('account_id')
    owner_config, owner_env = profile(pb, 'sso-owner')
    token = mint_token(args.owner_state, 'sso-owner-%d' % int(time.time()))
    run(pb, log, 'owner_login_token', [owner_config, 'login', '--api-token-file', '-'], owner_env, token)
    owner = lambda name, *a, **kw: run(pb, log, name, [owner_config, *a], owner_env, **kw)
    team_get = lambda name, **kw: run(pb, log, name, ['team', 'get', args.team, '--json'], **kw)
    try:
        team_get('member_team_get_before')
        review = owner('owner_enforce_review', 'team', 'sso', 'enforce', args.team, 'on', expect=2)
        confirm = re.search(r'--confirm (\S+)', review.stdout + review.stderr)
        owner('owner_enforce_on', 'team', 'sso', 'enforce', args.team, 'on', '--confirm', confirm.group(1) if confirm else '')
        team_get('member_team_get_refused', expect=1, code='team_sso_required')
        owner('owner_exempt_member', 'team', 'sso', 'exempt', args.team, member)
        team_get('exempt_member_team_get_works')
        owner('owner_exempt_remove', 'team', 'sso', 'exempt', args.team, member, '--remove')
        team_get('member_refused_after_exempt_removed', expect=1, code='team_sso_required')
    finally:  # leave the shared team the way it was
        owner('owner_enforce_off', 'team', 'sso', 'enforce', args.team, 'off')
    team_get('member_team_get_after_off')


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('mode', choices=['login', 'require-sso'])
    parser.add_argument('--team', required=True)
    parser.add_argument('--sso-state')
    parser.add_argument('--member-state')
    parser.add_argument('--owner-state')
    parser.add_argument('--out', required=True)
    args = parser.parse_args()
    pb = winhost.pb_path()
    if not winhost.device():
        parser.error('set PB_SMOKE_WINDOWS_PB and SMOKE_WINDOWS_SSH')
    log = Log(args.out)
    try:
        (sso_login if args.mode == 'login' else require_sso)(pb, log, args)
    except Exception as error:  # a step that cannot run is a failure with its reason in the receipt
        log.step('driver_error', args.mode, -1, False, None, str(error)[-200:])
    return log.verdict(mode=args.mode, team=args.team, build=whoami_version(pb))


def whoami_version(pb):
    return json.loads(pb.run_remote(['version', '--json'], timeout=40).stdout).get('data', {}).get('version')


if __name__ == '__main__':
    sys.exit(main())
