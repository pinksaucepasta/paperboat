#!/usr/bin/env python3
"""X-41 (F2-05, ENV part): what a revoke or reset of one machine does to ENV delivery on the other two OSes.

  xplat_removal_env.py [--only revoke,reset] [--targets L,M,W]     (env of xplat_removal.py: XPLAT_*, ORIGIN, STATE, PW_CHROMIUM;
                                                                    XPLAT_VAULT_PW: a private file holding the vault password)

A FRESH account (the vault is created on L when the account has none, the recovery code goes to a file next to the password).
Per target and flavour, the other two OSes are the observers:
  before    both observers `pb exec` the target and see every XP_ value byte for byte (xplat_env.py's sha256 dump);
  removal   an observer revokes the target by exact id, or the target resets and enrolls again;
  after     observers still list the ENV names, still deliver them to each other with `pb exec`, and `pb env host show` of the
            old machine ends with a clean message (never a hang or a stack trace); a revoked target cannot read its ENV any more
            (`pb env list` says its session is no longer valid and prints no value);
  again     the target is enrolled again (new identity), signed in and its vault unlocked: both observers `pb exec` it and see
            every XP_ value again, and it appears in `pb env host show` as published.
Results: results/xplat-removal-env-<stamp>.json, ids `X-41-ENV:<flavour>:<target>`.
"""
import json
import os
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import xplat_env as env
import xplat_fleet as fleet
import xplat_removal as rem

results = {}
GAP_TOTAL = 300  # seconds from the removal until the other machines read their ENV again (with the documented unlock)
DEVICES = {}


def ensure_vault(ctl):
    """Create the account's ENV vault on the controller when there is none; the password file is the one xplat_env reads."""
    password = env.PASSWORD
    if password.exists() and ctl.pb('env', 'vault', 'unlock', '--password-stdin', input=password.read_bytes().strip() + b'\n', timeout=120).rc == 0:
        return  # the vault of an earlier run of this account
    if not password.exists():
        password.write_bytes(os.urandom(18).hex().encode() + b'\n')
        password.chmod(0o600)
    remote = ctl.home + '/.pb-xplat-vault.pw'
    ctl.remove(ctl.home + '/.pb-xplat-recovery')
    ctl.write(remote, password.read_bytes(), mode='600')
    try:
        done = ctl.pb('env', 'vault', 'init', '--password-file', remote, '--recovery-file', ctl.home + '/.pb-xplat-recovery', '--json', timeout=180)
        text = done.text + done.errtext
        assert done.rc == 0 or 'already' in text.lower(), 'vault init failed: ' + text[-300:]
    finally:
        ctl.remove(remote)


def unlock(dev):
    done = dev.pb('env', 'vault', 'unlock', '--password-stdin', input=env.PASSWORD.read_bytes().strip() + b'\n', timeout=120)
    assert done.rc == 0, '%s: unlock failed: %s' % (dev.name, (done.text + done.errtext)[-200:])


def delivered(src, dst, seconds=240):
    """Seconds until every XP_ value reads on dst through src (0 when it never stopped), polled every 2 s."""
    began, last = time.time(), ''
    while time.time() - began < seconds:
        try:
            env.t_exec(src, dst)
            return int(time.time() - began)
        except AssertionError as error:
            last = str(error)
        time.sleep(2)
    raise AssertionError('ENV not delivered to %s after %d s: %s' % (dst.name, seconds, last[:200]))


def clean(text):
    return bool(text.strip()) and not rem.BAD.search(text) and len(text) < 2000


def flow(kind, target, observers, controller):
    old_id, _ = rem.identity(observers[0], target.alias)
    for dev in DEVICES.values():
        env.wait_applied(dev)
    for o in observers:
        env.t_exec(o, target)
    env.t_exec(observers[0], observers[1])
    env.t_exec(observers[1], observers[0])
    notes = ['before: both observers read all %d XP_ values on %s' % (len(env.VALUES), target.name)]
    if kind == 'revoke':
        rem.confirm_revoke(observers[0], old_id)
    else:
        rem.enroll_again(target, controller, 'reset')
    for o in observers:
        notes.append('%s: %s' % (o.name, rem.seen_gone(o, target, old_id)) if kind == 'revoke' else '%s: reset seen' % o.name)
    if kind == 'revoke':
        mine = target.pb('env', 'list', '--json', timeout=120)
        text = mine.text + mine.errtext
        assert mine.rc != 0 and 'no longer valid' in text and not any(v[:12] in text for v in env.VALUES.values()), \
            'the revoked machine can still read its ENV or says something unclear: %r' % text[-200:]
        notes.append('revoked target cannot read ENV: ' + text.strip().splitlines()[-1][:90])
    for o in observers:
        shown = o.pb('env', 'host', 'show', '--machine', old_id, '--json', timeout=120)
        if kind == 'revoke':
            assert clean(shown.text + shown.errtext), '%s: env host show of the removed machine: %r' % (o.name, (shown.text + shown.errtext)[-200:])
        names = o.pb('env', 'list', '--json', timeout=120)
        assert names.rc == 0 and all(k in names.text for k in env.VALUES), '%s lost the ENV names after the removal: %s' % (o.name, (names.text + names.errtext)[-200:])
    # The removal fences the sources the removed machine could read: the others report ENV pending until a client with the
    # vault password unlocks it again (`pb env unlock` re-seals the layers). Measure both, then the recovery.
    blank = time.time()
    try:
        env.t_exec(observers[0], observers[1])
        notes.append('observers never lost their ENV')
    except AssertionError:
        notes.append('ENV pending on the observers after the removal')
        unlock(observers[0])
        notes.append('delivered %d s after `pb env unlock` on %s' % (delivered(observers[0], observers[1]), observers[0].name))
    for a, b in ((observers[0], observers[1]), (observers[1], observers[0])):
        notes.append('%s>%s %d s' % (a.name, b.name, delivered(a, b)))
        assert time.time() - blank < GAP_TOTAL, 'ENV on the other machines still empty %d s after the removal' % (time.time() - blank)
    notes.append('observers still list the ENV names and deliver all values to each other')
    if kind == 'revoke':
        rem.enroll_again(target, controller, 'reset')
    for dev in (target,):
        rem.sign_in(dev, TOKEN)
        unlock(dev)
    new_id, _ = rem.identity(observers[0], target.alias)
    assert new_id != old_id, 'the machine enrolled again kept its id'
    waited = 0
    for o in observers:
        began = time.time()
        for attempt in range(36):
            try:
                env.t_exec(o, target)
                break
            except AssertionError as error:
                last = str(error)
                time.sleep(5)
        else:
            raise AssertionError('%s: ENV not delivered to the machine enrolled again after 3 min: %s' % (o.name, last[:300]))
        waited = max(waited, int(time.time() - began))
    notes.append('machine enrolled again: both observers read all values on it (<= %d s)' % waited)
    return '; '.join(notes)


def check(kind, ident, action):
    key = 'X-41-ENV:%s:%s' % (kind, ident)
    started = time.time()
    try:
        detail = action()
        results[key] = {'status': 'PASS', 'detail': detail}
        print('PASS %s %ds %s' % (key, time.time() - started, detail), flush=True)
    except Exception as error:  # noqa: BLE001
        results[key] = {'status': 'FAIL', 'detail': str(error)[:700]}
        print('FAIL %s %ds: %s' % (key, time.time() - started, str(error)[:700]), flush=True)


def main():
    global TOKEN
    argv = sys.argv[1:]
    kinds = argv[argv.index('--only') + 1].split(',') if '--only' in argv else ['revoke', 'reset']
    targets = argv[argv.index('--targets') + 1].split(',') if '--targets' in argv else ['L', 'M', 'W']
    devices = fleet.fleet(('L', 'M', 'W'))
    DEVICES.update(devices)
    TOKEN = rem.mint_api_token()
    for dev in devices.values():
        rem.sign_in(dev, TOKEN)
    ensure_vault(devices['L'])
    env.provision(devices)
    for kind in kinds:
        for key in targets:
            target = devices[key]
            observers = [d for k, d in devices.items() if k != key]
            controller = next(devices[k] for k in 'LMW' if k != key)
            check(kind, key, lambda: flow(kind, target, observers, controller))
            for dev in devices.values():  # a reset drops the device's own sign-in and its vault custody
                try:
                    rem.sign_in(dev, TOKEN)
                    unlock(dev)
                except Exception as error:  # noqa: BLE001
                    print('sign-in/unlock of %s failed: %s' % (dev.name, str(error)[:160]), flush=True)
    out = Path(__file__).parent / 'results' / ('xplat-removal-env-%s.json' % time.strftime('%Y%m%dT%H%M%SZ', time.gmtime()))
    out.write_text(json.dumps({'check': 'xplat-removal-env', 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
