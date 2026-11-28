#!/usr/bin/env python3
"""Two throwaway accounts on real devices and a paid team, for the team-scoped cross-OS rows (X-21, the team parts of
X-12 and X-19).

  xos_team.py up   STATE_DIR BUILD_DIR    accounts a (team owner) and b (member), their machines, one ENV vault each, a paid
                                          team (Stripe SANDBOX Checkout) that holds members' new machines for approval, b a member
  xos_team.py env  STATE_DIR              the shell exports xplat_team.py, xplat_envteam.py and xplat_team_private_end.py need
  xos_team.py rejoin STATE_DIR            b is made an active member again (xplat_envteam.py's revoke removes it)
  xos_team.py down STATE_DIR              team deleted, every machine uninstalled or revoked, both accounts deleted
  xos_team.py run  BUILD_DIR -- cmd...    up, run cmd with that environment, down on every exit (also on failure)

  account a: Linux (systemd container on XT_LINUX_HOST), macOS (XT_MAC_SSH), Windows (XT_WIN_A_SSH)
  account b: Linux (systemd container on XT_LINUX_HOST), Windows (XT_WIN_B_SSH)
Environment: those four ssh hosts (the macOS and Windows runners never had pb) and SMOKE_BROWSER_HOST (a Linux runner with
google-chrome: no browser runs on the controller). BUILD_DIR holds the three release binaries; its name (or CX_VERSION) is
the version. STATE_DIR is mode 700 and holds the accounts' sessions, logins and vault secrets (never printed) until `down`.
"""
import concurrent.futures
import json
import os
import secrets
import shlex
import shutil
import signal
import subprocess
import sys
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import xos_fleet  # noqa: E402

HOSTS = ('XT_LINUX_HOST', 'XT_MAC_SSH', 'XT_WIN_A_SSH', 'XT_WIN_B_SSH')
MACHINES = {'a': ('L', 'M', 'W'), 'b': ('L', 'W')}
CT_USER, CT_UID, CT_DOCKER = 'pbt', '1000', 'sudo -n docker'
say = xos_fleet.say


def meta_of(state):
    return json.loads((state / 'fleet.json').read_text())


def save(state, meta):
    (state / 'fleet.json').write_text(json.dumps(meta))


def name_of(meta, account, key):
    """test-xt-<run>-a-l ... for the owner (xplat_team.py: PREFIX + '-l'), test-xt-<run>-bl ... for the member (PREFIX + 'l')."""
    return meta['prefix'] + ('-a-' if account == 'a' else '-b') + key.lower()


def exports(state):
    meta = meta_of(state)
    names = {(a, k): name_of(meta, a, k) for a, keys in MACHINES.items() for k in keys}
    build = meta['build']
    return {
        'XPLAT_L_HOST': meta['XT_LINUX_HOST'], 'XPLAT_L_CT': names['a', 'L'], 'XPLAT_L_USER': CT_USER, 'XPLAT_L_UID': CT_UID,
        'XPLAT_L_DOCKER': CT_DOCKER, 'XPLAT_CONTROLLER': 'L', 'XPLAT_PREFIX': meta['prefix'] + '-a',
        'XPLAT_MAC_SSH': meta['XT_MAC_SSH'], 'SMOKE_WINDOWS_SSH': meta['XT_WIN_A_SSH'],
        'XPLAT_L_NAME': names['a', 'L'], 'XPLAT_M_NAME': names['a', 'M'], 'XPLAT_W_NAME': names['a', 'W'],
        'XPLAT_VAULT_PW': str(state / 'a' / 'vault.pw'), 'XPLAT_MEMBER_VAULT_PW': str(state / 'b' / 'vault.pw'),
        'XPLAT_TEAM': meta.get('team', ''), 'XPE_TEAM': meta.get('team', ''), 'XPE_OWNER_CTL_CT': names['a', 'L'],
        # xplat_team.py: the Mac is the owner's; each pair uses the owner's machine of OS A when the member has OS B, else the reverse
        'XPE_OWNER_HAS_MAC': '1', 'XPE_OTHER_OWNS': '', 'XPE_MEMBER_PREFIX': meta['prefix'] + '-b',
        'XPE_MEMBER_L_CT': names['b', 'L'], 'XPE_MEMBER_WIN_SSH': meta['XT_WIN_B_SSH'],
        # xplat_envteam.py: the member's Linux and Windows machines
        'XPLAT_MEMBER_CT': names['b', 'L'], 'XPLAT_MEMBER_NAME': names['b', 'L'],
        'XPLAT_MEMBER_WIN_SSH': meta['XT_WIN_B_SSH'], 'XPLAT_MEMBER_WIN_NAME': names['b', 'W'],
        # xplat_team_private_end.py
        'XPE_A_NAME': names['a', 'L'], 'XPE_B_NAME': names['b', 'L'], 'XPE_M_NAME': names['a', 'M'], 'XPE_W_NAME': names['b', 'W'],
        'XPE_BUILD': meta['version'], 'CX_VERSION': meta['version'],
    }


def devices(state):
    """{(account, key): xplat_fleet device}. The Linux machines are containers on the Linux host."""
    meta = meta_of(state)
    os.environ.update(exports(state))
    os.environ.pop('XPLAT_L_SSH', None)
    import xplat_fleet as fleet
    fleet.L_HOST = meta['XT_LINUX_HOST']
    made = {}
    for account in MACHINES:
        made[account, 'L'] = fleet.linux(name_of(meta, account, 'L'), ct=name_of(meta, account, 'L'))
    return fleet, made


def windows_and_mac(state, fleet, made):
    meta = meta_of(state)
    made['a', 'M'] = fleet.mac(name_of(meta, 'a', 'M'))
    made['a', 'W'] = fleet.Windows(name_of(meta, 'a', 'W'), target=meta['XT_WIN_A_SSH'])
    made['b', 'W'] = fleet.Windows(name_of(meta, 'b', 'W'), target=meta['XT_WIN_B_SSH'])


def ct_prefix(meta, account):
    """The shell prefix that runs ONE shell string in the account's Linux container (what fixture_checks.Machines.at takes)."""
    return 'ssh %s %s exec -i -u %s -e HOME=/home/%s -e XDG_RUNTIME_DIR=/run/user/%s -w /home/%s %s' % (
        meta['XT_LINUX_HOST'], CT_DOCKER, CT_USER, CT_USER, CT_UID, CT_USER, name_of(meta, account, 'L'))


def fixture_helpers(meta):
    """fixture_checks.Machines for paid_team(): its browser is the remote one, never one on the controller."""
    import fixture_checks
    helpers = fixture_checks.Machines()
    helpers.env.update(xos_fleet.browser_env())
    helpers.run_id = meta['prefix'].replace('test-', '')
    return fixture_checks, helpers


def container(state, meta, action, account, *args):
    env = dict(os.environ, **xos_fleet.browser_env(), SMOKE_CT_DOCKER=CT_DOCKER)
    return subprocess.run([str(HERE / 'fixture_machine.sh'), action, meta['XT_LINUX_HOST'], name_of(meta, account, 'L'), *args],
                          capture_output=True, text=True, timeout=900, env=env, stdin=subprocess.DEVNULL)


def pb_a(dev, *args, timeout=180):
    done = dev.pb(*args, timeout=timeout)
    if done.rc:
        raise RuntimeError('pb %s: %s' % (' '.join(args[:3]), (done.text + done.errtext)[-300:]))
    return done.text


def team_info(dev, team):
    value = json.loads(pb_a(dev, 'team', 'get', team, '--json'))
    return value.get('data', value)


def join_member(state, made):
    meta = meta_of(state)
    email = json.loads((state / 'b' / 'account' / 'account.json').read_text())['email']
    owner, member, team = made['a', 'L'], made['b', 'L'], meta['team']
    account = json.loads(pb_a(member, 'auth', 'status', '--json'))
    account = account.get('data', account)['account']['id']
    if any(m['account_id'] == account and m['active'] for m in team_info(owner, team)['members']):
        say('b is already a member')
        return
    done = owner.pb('team', 'invite', team, email, '--generation', str(team_info(owner, team)['generation']), '--json', timeout=180)
    if done.rc:
        raise RuntimeError('invite: ' + (done.text + done.errtext).replace(email, '<member>')[-300:])
    value = json.loads(done.text.strip().splitlines()[-1])
    pb_a(member, 'team', 'accept', value.get('data', value)['invitation_id'], '--json')
    say('b joined the team')


def up(state, build):
    missing = [name for name in HOSTS + ('SMOKE_BROWSER_HOST',) if not os.environ.get(name)]
    if missing:
        raise SystemExit('set ' + ', '.join(missing))
    build = Path(build).resolve()
    for binary in xos_fleet.BINARIES.values():
        if not (build / binary).is_file():
            raise SystemExit('no %s in %s' % (binary, build))
    state.mkdir(mode=0o700, parents=True, exist_ok=True)
    prefix = 'test-xt-' + secrets.token_hex(3)
    meta = {**{n: os.environ[n] for n in HOSTS}, 'prefix': prefix, 'build': str(build),
            'version': os.environ.get('CX_VERSION') or build.name, 'ids': {}}
    save(state, meta)
    for account in MACHINES:
        (state / account).mkdir(mode=0o700, exist_ok=True)
        for attempt in (1, 2):  # the sign-up code mail sometimes never arrives: one more account is tried
            try:
                xos_fleet.node(state / account, 'fresh_account.mjs', 'signup', str(state / account / 'account'))
                break
            except (RuntimeError, subprocess.TimeoutExpired):
                if attempt == 2:
                    raise
                shutil.rmtree(state / account / 'account', ignore_errors=True)
        say('account %s signed up' % account)
    for account in MACHINES:
        done = container(state, meta, 'up', account, str(build / xos_fleet.BINARIES['L']), str(state / account / 'account' / 'state.json'))
        if done.returncode:
            raise RuntimeError('container of %s: %s' % (account, done.stderr.strip()[-400:]))
        meta['ids'][name_of(meta, account, 'L')] = json.loads(done.stdout.strip().splitlines()[-1])['machine_id']
        save(state, meta)
        say('%s paired' % name_of(meta, account, 'L'))
    fleet, made = devices(state)
    windows_and_mac(state, fleet, made)
    with concurrent.futures.ThreadPoolExecutor(3) as pool:
        jobs = [pool.submit(xos_fleet.enroll, fleet, state / account, key, made[account, key], build / xos_fleet.BINARIES[key])
                for account, key in (('a', 'M'), ('a', 'W'), ('b', 'W'))]
        errors = [str(job.exception()) for job in jobs if job.exception()]  # waits for all three, so no install outlives a failed up
    if errors:
        raise RuntimeError('; '.join(errors))
    for account, keys in MACHINES.items():
        xos_fleet.make_vault(state / account, {key: made[account, key] for key in keys})
    checks, helpers = fixture_helpers(meta)
    meta['team'] = 'pbkey-' + helpers.run_id  # paid_team's id, recorded first so `down` removes it also after a half-done payment
    save(state, meta)
    team = checks.paid_team(helpers, ct_prefix(meta, 'a'))
    assert team == meta['team'], 'paid_team made %s, expected %s' % (team, meta['team'])
    say('paid team %s created (Stripe sandbox)' % team)
    pb_a(made['a', 'L'], 'team', 'machine', 'approval', team, 'on')
    join_member(state, made)
    say('fleet %s ready' % prefix)


def down(state):
    """Every step runs even when an earlier one failed; exit status 1 names what is left."""
    if not (state / 'fleet.json').is_file():
        shutil.rmtree(state, ignore_errors=True)
        return 0
    meta, left = meta_of(state), []
    fleet, made = devices(state)
    team = meta.get('team')
    if team and meta['ids'].get(name_of(meta, 'a', 'L')):
        owner, member = made['a', 'L'], made['b', 'L']
        try:
            if owner.pb('team', 'get', team, '--json', timeout=120).rc == 0:
                member.pb('team', 'leave', team, '--generation', str(team_info(owner, team)['generation']), '--json', timeout=180)
                pb_a(owner, 'team', 'delete', team, '--confirm', team, '--generation', str(team_info(owner, team)['generation']), '--json')
                say('team %s deleted' % team)
        except Exception as error:  # noqa: BLE001 - deleting the owner's account below deletes the team and cancels its subscription
            say('team %s not deleted by command (the owner account deletion removes it): %s' % (team, str(error)[-250:]))
    try:
        windows_and_mac(state, fleet, made)
    except BaseException as error:  # noqa: BLE001 - an unreachable runner must not stop the account deletion
        say('could not reach a macOS or Windows runner: %s' % str(error)[:200])

    def uninstall(item):
        (account, key), dev = item
        try:
            if dev.pb_path and dev.run([dev.pb_path, '--version'], timeout=60).rc == 0:
                fleet.uninstall(dev)
                say('%s uninstalled' % dev.name)
        except Exception as error:  # noqa: BLE001 - the account deletion below still revokes the machine
            say('%s not uninstalled (the account deletion revokes it): %s' % (dev.name, str(error)[:200]))

    with concurrent.futures.ThreadPoolExecutor(3) as pool:
        list(pool.map(uninstall, [item for item in made.items() if item[0][1] != 'L']))
    for account in MACHINES:
        done = container(state, meta, 'down', account, meta['ids'].get(name_of(meta, account, 'L'), ''))
        if done.returncode:
            say('container %s: %s (the account deletion revokes the machine)' % (name_of(meta, account, 'L'), done.stderr.strip()[-200:]))
        else:
            say('%s revoked and removed' % name_of(meta, account, 'L'))
    for account in ('b', 'a'):
        if not (state / account / 'account' / 'state.json').is_file():
            continue
        for attempt in (1, 2, 3):
            try:
                xos_fleet.node(state / account, 'fresh_account.mjs', 'delete', str(state / account / 'account'))
                say('account %s deleted' % account)
                (state / account / 'account' / 'state.json').unlink()
                break
            except (RuntimeError, subprocess.TimeoutExpired) as error:
                if attempt == 3:
                    left.append('account %s NOT deleted, its login stays in %s: %s' % (account, state / account / 'account', str(error)[-200:]))
                time.sleep(20)
    if left:
        print('LEFTOVER ' + '; '.join(left), file=sys.stderr, flush=True)
        return 1
    shutil.rmtree(state, ignore_errors=True)
    return 0


def main():
    argv = sys.argv[1:]
    if len(argv) >= 2 and argv[0] == 'env':
        for key, value in exports(Path(argv[1]).resolve()).items():
            print('export %s=%s' % (key, shlex.quote(value)))
        return 0
    if len(argv) == 2 and argv[0] == 'down':
        return down(Path(argv[1]).resolve())
    if len(argv) == 2 and argv[0] == 'rejoin':
        state = Path(argv[1]).resolve()
        join_member(state, devices(state)[1])
        return 0
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(143))  # a killed run still removes its team and accounts
    signal.signal(signal.SIGHUP, lambda *_: sys.exit(129))
    if len(argv) == 3 and argv[0] == 'up':
        state = Path(argv[1]).resolve()
        try:
            up(state, argv[2])
        except BaseException:
            import traceback
            traceback.print_exc()
            down(state)
            raise SystemExit(1)
        return 0
    if len(argv) > 3 and argv[0] == 'run' and argv[2] == '--':
        import tempfile
        state = Path(tempfile.mkdtemp(prefix='xos-team-'))
        code = 1
        try:
            up(state, argv[1])
            code = subprocess.run(argv[3:], env=dict(os.environ, **exports(state))).returncode
        finally:
            code = max(code, down(state))
        return code
    raise SystemExit(__doc__)


if __name__ == '__main__':
    sys.exit(main())
