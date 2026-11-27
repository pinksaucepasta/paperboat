#!/usr/bin/env python3
"""A Linux, macOS and Windows fleet of a throwaway account, for the cross-OS rows that need a fresh account (ENV vault,
reset, removal) and so that no run depends on, or fills up, a retained account.

  xos_fleet.py up   STATE_DIR BUILD_DIR    new console account (fresh_account.mjs), the build installed on the three runners,
                                           each paired as test-cx-<run>-{l,m,w}, the ENV vault made and unlocked on all three
  xos_fleet.py env  STATE_DIR              the shell exports the cross-OS scripts need for that fleet
  xos_fleet.py down STATE_DIR              uninstall the three and delete the account (which revokes its machines)
  xos_fleet.py run  BUILD_DIR -- cmd...    up, run cmd with that environment, down on every exit (also on failure)

Environment: XPLAT_L_SSH, XPLAT_MAC_SSH, SMOKE_WINDOWS_SSH (three runners that never had pb), SMOKE_BROWSER_HOST (a
Linux runner with google-chrome, not one of the three: no browser runs on the controller). BUILD_DIR holds
pb-linux-amd64, pb-darwin-arm64 and pb-windows-amd64.exe. STATE_DIR is made mode 700; it holds the account's session,
its login and the vault password and recovery code (all 600, never printed) until `down` removes it.
"""
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
import dev_targets  # noqa: E402

BINARIES = {'L': 'pb-linux-amd64', 'M': 'pb-darwin-arm64', 'W': 'pb-windows-amd64.exe'}
HOSTS = ('XPLAT_L_SSH', 'XPLAT_MAC_SSH', 'SMOKE_WINDOWS_SSH')


def say(text):
    print('xos_fleet %s %s' % (time.strftime('%H:%M:%S', time.gmtime()), text), flush=True)


def browser_env():
    settings = dev_targets.smoke_environment(dev_targets.load())
    if not os.environ.get('SMOKE_BROWSER_HOST'):
        raise SystemExit('SMOKE_BROWSER_HOST is not set: the sign-up browser runs on a pool runner, never here')
    env = {'ORIGIN': settings['SMOKE_CONSOLE_ORIGIN'], 'PW_CHROMIUM': str(HERE / 'remote_chrome.sh'),
           'SMOKE_BROWSER_HOST': os.environ['SMOKE_BROWSER_HOST']}
    if settings.get('SMOKE_PW_MODULE'):
        env['PW_MODULE'] = os.path.expanduser(settings['SMOKE_PW_MODULE'])
    return env


def node_env(state):
    return dict(os.environ, STATE=str(state / 'account' / 'state.json'), **browser_env())


def node(state, script, *args, timeout=900):
    done = subprocess.run(['node', str(HERE / script), *args], capture_output=True, text=True, timeout=timeout,
                          stdin=subprocess.DEVNULL, env=node_env(state))
    if done.returncode:
        raise RuntimeError('%s failed: %s' % (script, (done.stdout + done.stderr).strip()[-400:]))
    return done.stdout


def exports(state):
    meta = json.loads((state / 'fleet.json').read_text())
    return {'XPLAT_L_SSH': meta['XPLAT_L_SSH'], 'XPLAT_L_HOST': meta['XPLAT_L_SSH'], 'XPLAT_MAC_SSH': meta['XPLAT_MAC_SSH'],
            'SMOKE_WINDOWS_SSH': meta['SMOKE_WINDOWS_SSH'], 'XPLAT_CONTROLLER': 'L', 'XPLAT_PREFIX': meta['prefix'],
            'XPLAT_L_NAME': meta['prefix'] + '-l', 'XPLAT_M_NAME': meta['prefix'] + '-m', 'XPLAT_W_NAME': meta['prefix'] + '-w',
            'XPLAT_VAULT_PW': str(state / 'vault.pw'), 'XPLAT_VAULT_RECOVERY': str(state / 'vault.recovery'),
            'XPLAT_BUILD_DIR': meta['build'], 'CX_VERSION': meta['version'],
            # the removal rows mint API tokens with the account's console session; reinstalls use the same builds
            'STATE': str(state / 'account' / 'state.json'), **{k: v for k, v in browser_env().items()},
            'XPLAT_L_ARCH': 'amd64', 'XPLAT_BUILD_L': meta['build'] + '/' + BINARIES['L'], 'XPLAT_BUILD_M': meta['build'] + '/' + BINARIES['M'],
            'XPLAT_BUILD_W': meta['build'] + '/' + BINARIES['W']}


def devices(state):
    os.environ.update(exports(state))
    import xplat_fleet as fleet
    return fleet, fleet.fleet(('L', 'M', 'W'))


def enroll(fleet, state, key, dev, binary):
    """Install the build and pair it with a one-shot token of the throwaway account (written to a private file, never printed)."""
    data = fleet.install(dev, binary)
    say('%s installed %s' % (key, data.get('version', '')))
    token = node(state, 'mint_enrollment_token.mjs', timeout=180).encode()
    token_file = dev.home + ('\\.pb-xos-token' if dev.os == 'windows' else '/.pb-xos-token')
    dev.write(token_file, token, mode='600')
    try:
        if dev.os == 'windows':
            fleet.winhost.RemotePath(dev.device, token_file).enrollment_token_file()
        done = dev.pb('pair', '--enrollment-token-file', token_file, '--name', dev.name, '--json', timeout=600)
        value = json.loads(done.text.strip().splitlines()[-1]) if done.text.strip() else {}
        if not value.get('ok'):
            raise RuntimeError('%s pair failed: %s' % (dev.name, (done.text + done.errtext)[-300:]))
    finally:
        dev.remove(token_file)
    say('%s paired as %s' % (key, dev.name))


def make_vault(state, targets):
    password = secrets.token_hex(16) + 'Aa1!'
    path = state / 'vault.pw'
    path.write_text(password)
    path.chmod(0o600)
    ctl = targets['L']
    recovery = ctl.home + '/.pb-xos-recovery'
    ctl.remove(recovery)
    done = ctl.pb('env', 'init', '--password-stdin', '--recovery-file', recovery, '--json', input=password.encode() + b'\n', timeout=240)
    if done.rc:
        raise RuntimeError('pb env init failed: ' + (done.text + done.errtext)[-300:])
    code = ctl.read(recovery)
    ctl.remove(recovery)
    (state / 'vault.recovery').write_bytes(code or b'')
    (state / 'vault.recovery').chmod(0o600)
    for key, dev in targets.items():
        done = None
        for _ in range(12):  # another machine sees the new vault after its next sync
            done = dev.pb('env', 'unlock', '--password-stdin', input=password.encode() + b'\n', timeout=150)
            if done.rc == 0:
                break
            time.sleep(10)
        if done.rc:
            raise RuntimeError('%s: pb env unlock failed: %s' % (key, (done.text + done.errtext)[-300:]))
        say('%s vault unlocked' % key)


def up(state, build):
    missing = [name for name in HOSTS if not os.environ.get(name)]
    if missing:
        raise SystemExit('set ' + ', '.join(missing))
    build = Path(build).resolve()
    for name in BINARIES.values():
        if not (build / name).is_file():
            raise SystemExit('no %s in %s' % (name, build))
    version = os.environ.get('CX_VERSION') or build.name
    state.mkdir(mode=0o700, parents=True, exist_ok=True)
    prefix = 'test-cx-' + secrets.token_hex(3)  # never test-NNNN-*: those names are the protected fleet's
    (state / 'fleet.json').write_text(json.dumps({**{n: os.environ[n] for n in HOSTS}, 'prefix': prefix, 'build': str(build), 'version': version}))
    for attempt in (1, 2):  # the sign-up code mail sometimes never arrives: one more account is tried
        try:
            node(state, 'fresh_account.mjs', 'signup', str(state / 'account'))
            break
        except (RuntimeError, subprocess.TimeoutExpired):
            if attempt == 2:
                raise
            shutil.rmtree(state / 'account', ignore_errors=True)
    say('throwaway account signed up')
    fleet, targets = devices(state)
    for key, dev in targets.items():  # L first: it is the controller
        enroll(fleet, state, key, dev, build / BINARIES[key])
    make_vault(state, targets)
    say('fleet %s ready' % prefix)


def down(state):
    """Every step runs even when an earlier one failed; exit status 1 names what is left."""
    left = []
    if (state / 'fleet.json').is_file():
        fleet, targets = devices(state)
        for key, dev in targets.items():
            try:
                fleet.uninstall(dev)
                say('%s uninstalled' % key)
            except Exception as error:  # noqa: BLE001 - the account deletion below still revokes the machine
                say('%s not uninstalled (the account deletion revokes it): %s' % (key, str(error)[:200]))
    if (state / 'account' / 'state.json').is_file():
        for attempt in (1, 2, 3):
            try:
                node(state, 'fresh_account.mjs', 'delete', str(state / 'account'))
                say('throwaway account deleted')
                break
            except (RuntimeError, subprocess.TimeoutExpired) as error:
                if attempt == 3:
                    left.append('throwaway account NOT deleted, its login stays in %s: %s' % (state / 'account', str(error)[-200:]))
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
    if len(argv) == 3 and argv[0] == 'up':
        state = Path(argv[1]).resolve()
        try:
            up(state, argv[2])
        except BaseException:
            down(state)
            raise
        return 0
    if len(argv) > 3 and argv[0] == 'run' and argv[2] == '--':
        import tempfile
        state = Path(tempfile.mkdtemp(prefix='xos-fleet-'))
        signal.signal(signal.SIGTERM, lambda *_: sys.exit(143))  # a killed run still removes its account
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
