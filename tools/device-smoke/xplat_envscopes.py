#!/usr/bin/env python3
"""X-12: ENV scopes across OSes — override isolation, the cross-scope case rule, host show, rotation after a revoke.

  XPLAT_PREFIX=xa2 ... xplat_envscopes.py [--devices L,M,W] [--only override,personal,case,hoststate,rotation]

Runs on the fleet of a fresh account (xplat_fleet.py up) whose ENV vault password is in XPLAT_VAULT_PW (read inside, never
printed). Values are random per run and never printed. Results: results/xplat-envscopes-<stamp>.json (`X-12:override:W`, ...).
  override   a per-device override set for one machine reaches only that machine (each device in turn is the target)
  personal   a personal value reaches every device
  case       personal `Name` plus an override `NAME` for the same machine: Windows has ONE variable (the override wins),
             Linux and macOS keep two variables with their own values
  hoststate  `pb env host show` says published + applied on each device once it applied, and `pb env names` names the variable
  rotation   a throwaway Linux machine is revoked: every survivor shows a pending delivery (not published, not applied), says
             so on a command, is re-sealed by unlocking the vault, applied again at a newer fence, and sees the value; the
             revoked machine is not given the rotated delivery
"""
import base64
import json
import os
import subprocess
import sys
import time
import uuid
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import xplat_fleet as fleet  # noqa: E402

results = {}
TAG = uuid.uuid4().hex[:6]
PASSWORD = Path(os.environ.get('XPLAT_VAULT_PW', Path.home() / '.pbmac/xplat2-vault.pw'))
WAIT = 180

DUMP_UNIX = 'import os,json\nprint(json.dumps({k: v for k, v in os.environ.items() if k.upper().startswith("XS_")}))\n'
DUMP_WINDOWS = ("Get-ChildItem env: | Where-Object { $_.Name -like 'XS_*' } | ForEach-Object { $_.Name + '=' + $_.Value } | "
                "ForEach-Object { Write-Output $_ }")


def observe(ctl, dev):
    """The XS_ variables a new `pb exec` process on `dev` receives, with the spelling the process has."""
    if dev.os == 'windows':
        script = base64.b64encode(DUMP_WINDOWS.encode('utf-16-le')).decode()
        done = ctl.pb('exec', dev.alias, '--', 'powershell', '-NoProfile', '-NonInteractive', '-EncodedCommand', script, timeout=180)
        pairs = [line.split('=', 1) for line in done.text.splitlines() if line.startswith('XS_') and '=' in line]
        return done.rc, {k: v.strip() for k, v in pairs}
    done = ctl.pb('exec', dev.alias, '--', 'python3', '-c', DUMP_UNIX, timeout=180)
    return done.rc, json.loads(done.text.strip().splitlines()[-1]) if done.rc == 0 and done.text.strip() else {}


def until(ctl, dev, want, seconds=WAIT):
    """Poll a new process on `dev` until the XS_ variables equal `want`; returns the last observation."""
    end, seen = time.time() + seconds, None
    while True:
        rc, seen = observe(ctl, dev)
        if rc == 0 and seen == want:
            return seen
        if time.time() > end:
            return seen
        time.sleep(6)


def unlock(dev):
    return dev.pb('env', 'vault', 'unlock', '--password-stdin', input=PASSWORD.read_bytes().strip() + b'\n', timeout=120)


def setv(ctl, name, value, machine=None):
    args = ['env', 'set', name, '--value-stdin'] + (['--machine', machine] if machine else [])
    done = ctl.pb(*args, input=value.encode(), timeout=120)
    if done.rc != 0 and 'locked' in done.text + done.errtext:  # a Linux machine without a Secret Service forgets the unlock after 15 minutes
        unlock(ctl)
        done = ctl.pb(*args, input=value.encode(), timeout=120)
    assert done.rc == 0, 'set %s%s: %s' % (name, ' on ' + machine if machine else '', (done.text + done.errtext)[-200:])


def unset(ctl, name, machine=None):
    base = ['env', 'unset', name] + (['--machine', machine] if machine else []) + ['--json']
    if 'locked' in ctl.pb(*base, timeout=120).text:
        unlock(ctl)
    token = (json.loads(ctl.pb(*base, timeout=120).text or '{}').get('data') or {}).get('confirmation_token')
    if token:
        ctl.pb(*base, '--confirm', token, timeout=120)


def t_override(ctl, devices, target):
    name = 'XS_OV_' + TAG
    want = {d.name: 'ov-%s-%s' % (d.name, TAG) for d in devices.values()}
    try:
        for dev in devices.values():
            setv(ctl, name, want[dev.name], machine=dev.alias)
        bad = []
        for dev in devices.values():
            seen = until(ctl, dev, {name: want[dev.name]})
            if seen != {name: want[dev.name]}:
                bad.append('%s sees %s' % (dev.name, sorted(seen or {})))
        assert not bad, 'an override is missing on its machine or leaked: ' + '; '.join(bad)
        return 'each device sees only its own override: %s' % ', '.join(sorted(want))
    finally:
        for dev in devices.values():
            unset(ctl, name, machine=dev.alias)


def t_personal(ctl, devices):
    name = 'XS_PE_' + TAG
    value = 'personal-' + TAG
    try:
        setv(ctl, name, value)
        bad = [d.name for d in devices.values() if until(ctl, d, {name: value}) != {name: value}]
        assert not bad, 'the personal value did not reach: %s' % bad
        return 'the personal value reaches %s' % ', '.join(d.name for d in devices.values())
    finally:
        unset(ctl, name)


def t_case(ctl, devices, dev):
    mixed, upper = 'XS_Case_' + TAG, 'XS_CASE_' + TAG
    try:
        setv(ctl, mixed, 'personal-' + TAG)
        setv(ctl, upper, 'override-' + TAG, machine=dev.alias)
        if dev.os == 'windows':
            want, why = {upper: 'override-' + TAG}, 'one variable, the device override wins'
        else:
            want, why = {mixed: 'personal-' + TAG, upper: 'override-' + TAG}, 'two variables, each with its own value'
        seen = until(ctl, dev, want)
        assert seen == want, '%s: expected %s, got %s' % (dev.name, sorted(want.items()), sorted((seen or {}).items()))
        return '%s: %s' % (dev.name, why)
    finally:
        unset(ctl, mixed)
        unset(ctl, upper, machine=dev.alias)


def t_hoststate(ctl, dev):
    name = 'XS_HS_' + TAG
    try:
        setv(ctl, name, 'hs-' + TAG)
        assert until(ctl, dev, {name: 'hs-' + TAG}) == {name: 'hs-' + TAG}, 'the value never reached %s' % dev.name
        show = json.loads(dev.pb('env', 'host', 'show', '--machine', dev.alias, '--json', timeout=120).text)
        show = show.get('data', show)
        assert show.get('published') and show.get('applied'), 'host show on %s after a launch: published=%s applied=%s' % (dev.name, show.get('published'), show.get('applied'))
        names = dev.pb('env', 'names', timeout=120).text.split()
        assert name in names, '`pb env names` on %s does not name %s' % (dev.name, name)
        return 'host show: published and applied; `pb env names` names the variable on %s' % dev.name
    finally:
        unset(ctl, name)


def fence(show):
    return max([layer['fence_generation'] for layer in show.get('layers', [])] or [0])


def host(ctl, dev_or_id):
    done = ctl.pb('env', 'host', 'show', '--machine', dev_or_id, '--json', timeout=120)
    if done.rc != 0:
        return None
    value = json.loads(done.text)
    return value.get('data', value)


def true_argv(dev):
    return ['python', '-c', 'pass'] if dev.os == 'windows' else ['python3', '-c', 'pass']


def throwaway(prefix, container):
    """A disposable Linux container paired to the account: returns (device, machine id)."""
    host_ssh = os.environ.get('XPLAT_ROT_HOST', fleet.L_HOST)  # a host whose docker has the systemd image and the arch of XPLAT_ROT_ARCH
    docker = os.environ.get('XPLAT_L_DOCKER', 'docker').split()
    sh = lambda script: subprocess.run(['ssh', host_ssh, script], capture_output=True, text=True, timeout=240)  # noqa: E731
    d = ' '.join(docker)
    stale = fleet.machines().get(prefix + '-rot')  # a record left by an interrupted run would make the server name this one '-rot-2' and every exec by alias would hit the offline one
    if stale:
        revoke(fleet.controller(), stale['id'])
    sh('%s rm -f %s; %s run -d --name %s --hostname %s --privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock f1407s-systemd' % (d, container, d, container, container))
    time.sleep(8)
    sh("%s exec %s bash -c \"useradd -m -u 1001 -s /bin/bash tester && echo 'tester ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/tester && loginctl enable-linger tester && systemctl start user@1001.service\"" % (d, container))
    controller_host, fleet.L_HOST = fleet.L_HOST, host_ssh
    try:
        dev = fleet.linux(name=prefix + '-rot', ct=container)
    finally:
        fleet.L_HOST = controller_host
    fleet.install(dev, fleet.BUILD_DIR / ('pb-linux-' + os.environ.get('XPLAT_ROT_ARCH', os.environ.get('XPLAT_L_ARCH', 'amd64'))))
    machine_id = fleet.pair(dev, 'unix')
    return dev, machine_id


def revoke(ctl, machine_id):
    token = json.loads(ctl.pb('machine', 'revoke', machine_id, '--json').text)['data']['confirmation_token']
    done = ctl.pb('machine', 'revoke', machine_id, '--json', '--confirm', token)
    assert done.rc == 0, 'revoke failed: ' + (done.text + done.errtext)[-200:]


def t_rotation(ctl, devices):
    name, value = 'XS_ROT_' + TAG, 'rotation-' + TAG
    container = os.environ.get('XPLAT_ROT_CT', fleet.PREFIX + '-rot')
    revoked = machine_id = None
    try:
        setv(ctl, name, value)
        revoked, machine_id = throwaway(fleet.PREFIX, container)
        assert ctl.pb('env', 'vault', 'unlock', '--password-file', PASSWORD_REMOTE(ctl), '--json', timeout=120).rc == 0, 'unlock to seal for the new machine failed'
        assert until(ctl, revoked, {name: value}) == {name: value}, 'the throwaway machine never received the value'
        before = {}
        for dev in devices.values():
            assert until(ctl, dev, {name: value}) == {name: value}, '%s never received the value before the revoke' % dev.name
            before[dev.name] = fence(host(ctl, dev.alias))
        revoke(ctl, machine_id)
        pending = {}
        for dev in devices.values():
            end = time.time() + WAIT
            while time.time() < end:
                ctl.pb('exec', dev.alias, '--', *true_argv(dev), timeout=120)  # a launch makes the host report its state
                show = host(ctl, dev.alias)
                if show and not show['published'] and not show['applied'] and fence(show) > before[dev.name]:
                    pending[dev.name] = True
                    break
                time.sleep(4)
            assert pending.get(dev.name), '%s never showed a pending delivery after the revoke' % dev.name
            done = ctl.pb('exec', dev.alias, '--', *true_argv(dev), timeout=120)
            assert done.rc == 0 and 'ENV not applied' in done.errtext, '%s: a command while pending did not run and say that ENV was not applied: %s' % (dev.name, done.errtext[-150:])
        unlock = ctl.pb('env', 'vault', 'unlock', '--password-file', PASSWORD_REMOTE(ctl), '--json', timeout=120)
        assert unlock.rc == 0, 'unlock failed: ' + (unlock.text + unlock.errtext)[-150:]
        for dev in devices.values():
            assert until(ctl, dev, {name: value}) == {name: value}, '%s does not see the value after the rotation' % dev.name
            show = host(ctl, dev.alias)
            assert show['published'] and show['applied'] and fence(show) > before[dev.name], '%s: not re-sealed at a newer fence (%s -> %s)' % (dev.name, before[dev.name], fence(show))
        gone = host(ctl, machine_id)
        assert not gone or not any(layer['published'] and layer['fence_generation'] >= max(fence(host(ctl, d.alias)) for d in devices.values()) for layer in gone.get('layers', [])), 'the revoked machine was given the rotated delivery'
        return 'revoke: pending on %s, re-sealed by unlock at newer fences, value delivered, revoked machine not given the rotation' % ', '.join(devices)
    finally:
        unset(ctl, name)
        if machine_id:
            try:
                revoke(ctl, machine_id)
            except Exception:  # noqa: BLE001 already revoked by the check
                pass
        subprocess.run(['ssh', fleet.L_HOST] + os.environ.get('XPLAT_L_DOCKER', 'docker').split() + ['rm', '-f', container], capture_output=True, timeout=120)


def PASSWORD_REMOTE(ctl):
    """The password file on the controller (a private file written for this call, removed by the caller's device cleanup)."""
    path = ctl.home + '/.xplat-vault-pw'
    ctl.write(path, PASSWORD.read_bytes().strip() + b'\n', mode='600')  # the vault password was set through stdin with its newline
    return path


def check(name, who, action):
    key = 'X-12:%s:%s' % (name, who)
    started = time.time()
    try:
        detail = action()
        results[key] = {'status': 'PASS', 'detail': detail}
        print('PASS %s %ds %s' % (key, time.time() - started, detail[:300]), flush=True)
    except Exception as error:  # noqa: BLE001
        results[key] = {'status': 'FAIL', 'detail': str(error)[:700]}
        print('FAIL %s %ds: %s' % (key, time.time() - started, str(error)[:700]), flush=True)


def main():
    argv = sys.argv[1:]
    keys = argv[argv.index('--devices') + 1].split(',') if '--devices' in argv else ['L', 'M', 'W']
    only = set(argv[argv.index('--only') + 1].split(',')) if '--only' in argv else set()
    devices = {k: d for k, d in fleet.fleet().items() if k in keys}
    ctl = fleet.controller()
    password = PASSWORD.read_bytes().strip()
    for dev in devices.values():
        done = dev.pb('env', 'vault', 'unlock', '--password-stdin', input=password + b'\n', timeout=120)
        assert done.rc == 0, '%s: unlock failed' % dev.name
    for scope in [None] + [d.alias for d in devices.values()]:  # leftovers of an interrupted run would leak into the exact comparisons below
        listed = ctl.pb('env', 'list', *(['--machine', scope] if scope else []), '--json', timeout=120)
        for stale in (json.loads(listed.text).get('data', {}).get('names', []) if listed.rc == 0 and listed.text.strip() else []):
            if stale.startswith('XS_'):
                unset(ctl, stale, machine=scope)
    want = lambda n: not only or n in only  # noqa: E731
    if want('override'):
        for key, dev in devices.items():
            check('override', key, lambda: t_override(ctl, devices, dev))
    if want('personal'):
        check('personal', '+'.join(devices), lambda: t_personal(ctl, devices))
    if want('case'):
        for key, dev in devices.items():
            check('case', key, lambda: t_case(ctl, devices, dev))
    if want('hoststate'):
        for key, dev in devices.items():
            check('hoststate', key, lambda: t_hoststate(ctl, dev))
    if want('rotation'):
        check('rotation', '+'.join(devices), lambda: t_rotation(ctl, devices))
    ctl.remove(ctl.home + '/.xplat-vault-pw')
    stamp = time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())
    out = Path(__file__).parent / 'results' / ('xplat-envscopes-%s.json' % stamp)
    out.write_text(json.dumps({'check': 'xplat-envscopes', 'devices': keys, 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
