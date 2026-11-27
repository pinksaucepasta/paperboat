#!/usr/bin/env python3
"""Sign-in preflight: a CLI profile a check needs is signed in, proven by a call the server answers.

`pb auth status` is not that proof: it reads the local profile and says signed in for an expired session. The proof is
`pb team list --json`, as the device's own account (`default`) or as its second profile (`teammate`, teammate_profile.py).
This only checks: it never mints, reads or moves a token, and never signs anything in or out.

Run by windows_checks.py (imported) and smoke.sh (staged on the device, or here against the Windows device's pb.exe):
  profile_preflight.py --machine NAME [--container HOST:NAME] [--windows-install-json FILE] default|teammate ...
Prints `PROFILE <profile> OK` or `PROFILE <profile> <why, and the command that signs it in>` per profile; exit 0.
"""
import argparse
import json
import re
import subprocess
import sys
import time

from teammate_profile import environment, profile_root
import winhost

DEFAULT, TEAMMATE = 'default', 'teammate'
PROBE = ('team', 'list', '--json')  # a call the server must answer
LABELS = {DEFAULT: 'default', TEAMMATE: 'second (teammate)'}


def answer(returncode, stdout):
    """None when the server answered the probe; else why not. Only pb's error code is repeated, never its output."""
    if returncode == 0:
        return None
    try:
        code = str(json.loads(stdout)['error']['code'])
    except (ValueError, KeyError, TypeError):
        code = ''
    return '`pb team list` failed: %s' % (code if re.fullmatch(r'[a-z0-9_]{1,64}', code) else 'exit %d' % returncode)


def probe(profile):
    """One attempt on the machine under test: this one, or the Windows device."""
    pb = winhost.pb_path()
    arguments, env = list(PROBE), None
    if profile == TEAMMATE:
        root = profile_root()
        config = root / 'config.json'
        if not config.exists():
            return 'it was never signed in on this machine (it has no profile config.json)'
        arguments, env = ['--config', str(config), *PROBE], environment(root)
    if winhost.device():
        done = pb.run_remote(arguments, input='', env=env, timeout=60)
    else:
        done = subprocess.run([str(pb), *arguments], env=env, capture_output=True, text=True, timeout=60,
                              stdin=subprocess.DEVNULL)
    return answer(done.returncode, done.stdout)


def problem(attempt, attempts=2, sleep=time.sleep):
    """None once attempt() answers None; else its last reason. Whatever stops the probe, the profile is not proven
    signed in: that is a reason too, never a traceback."""
    for number in range(attempts):
        try:
            reason = attempt()
        except Exception as error:  # noqa: BLE001 the caller turns the reason into the check's FAIL row
            reason = 'the probe could not run: %s' % type(error).__name__
        if reason is None:
            return None
        if number + 1 < attempts:
            sleep(5)
    return reason


def signin_command(profile, machine, windows_pb='', install_json='', container='', container_user='pbt'):
    """How the operator signs that profile in (nothing here runs it)."""
    if profile == DEFAULT:
        if windows_pb:
            return "on %s, in PowerShell: & '%s' login" % (machine, windows_pb)
        if container:
            host, name = container.split(':', 1)
            return 'ssh -t %s docker exec -it -u %s %s /home/%s/.local/bin/pb login' % (host, container_user, name,
                                                                                      container_user)
        return 'on %s: pb login' % machine
    account = "ORIGIN=... STATE=<console session of that profile's account> PW_CHROMIUM=... "
    if windows_pb:
        return ('SMOKE_WINDOWS_SSH=<ssh login of %s> SMOKE_WINDOWS_PASSWORD=... %spython3 tools/device-smoke/teammate_signin.py '
                '--windows-install-json %s' % (machine, account, install_json or '<pb install --json receipt>'))
    if container:
        host, name = container.split(':', 1)
        return '%spython3 tools/device-smoke/teammate_signin.py %s %s %s' % (account, host, name, container_user)
    return ("on %s: python3 tools/device-smoke/teammate_login.py, with an API token of that profile's account on stdin "
            '(teammate_signin.py drives only containers and the Windows device)' % machine)


def detail(profile, machine, reason, command):
    """The FAIL row's text."""
    return 'sign-in preflight: the %s profile on %s is signed out (%s); sign it in: %s' % (
        LABELS.get(profile, profile), machine, reason, command)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.split('\n')[0])
    parser.add_argument('--machine', required=True)
    parser.add_argument('--container', default='', help='HOST:NAME when the machine is a container')
    parser.add_argument('--container-user', default='pbt')
    parser.add_argument('--windows-install-json', default='')
    parser.add_argument('profiles', nargs='+', choices=(DEFAULT, TEAMMATE))
    args = parser.parse_args(argv)
    for profile in args.profiles:
        reason = problem(lambda: probe(profile))
        windows_pb = str(winhost.pb_path()) if winhost.device() else ''
        print('PROFILE %s %s' % (profile, 'OK' if reason is None else detail(profile, args.machine, reason, signin_command(
            profile, args.machine, windows_pb, args.windows_install_json, args.container, args.container_user))), flush=True)
    return 0


if __name__ == '__main__':
    sys.exit(main())
