#!/usr/bin/env python3
"""X-41 (F1-09, F1-10, F2-05, config-sync part): what a revoke or reset of one machine does to the config-sync entries.

  xplat_removal_config.py [--only revoke,reset] [--targets L,M,W]     (env of xplat_config.py: XPLAT_REPO_*, XPLAT_MAC_SSH, ...)

One repository, one assignment per device (xplat_config.Context). For each target and flavour the other two OSes are observers:
  before    a settings.json written on observer 1 arrives byte for byte on observer 2 and on the target; every device's
            `pb config status` lists its own assignment;
  removal   an observer revokes the target by exact id, or the target resets and enrolls again;
  after     within 2 minutes no observer lists a config entry for the removed machine's alias online, a second edit still
            travels between the observers, and the observers' status never reports an error for the missing peer;
  again     the machine enrolled again starts with NO assignment (nothing silently inherited from the old identity); assigning it
            again makes the next edit arrive on it and an edit made on it arrive on the observers.
Results: results/xplat-removal-config-<stamp>.json, ids `X-41-CFG:<flavour>:<target>`.
"""
import json
import os
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import xplat_config as x
import xplat_fleet as fleet
import xplat_removal as rem

results = {}
SETTINGS = ('settings.json',)


def aliases(dev):
    """Aliases of every config environment this device's status lists."""
    return {e.get('alias'): e for e in x.pbj(dev, 'config', 'status').get('environments', [])}


def edit(src, dsts, tag):
    data = ('{"x41": "%s", "salt": "%s"}\n' % (tag, os.urandom(8).hex())).encode()
    src.write(x.real(src, *SETTINGS), data)
    for dst in dsts:
        try:
            x.arrives(dst, x.real(dst, *SETTINGS), data, '%s edit %s to reach %s' % (src.name, tag, dst.name))
        except AssertionError as error:  # what the receiver says about itself, so a miss names its cause
            mine = aliases(dst).get(dst.name, {})
            keep = {k: mine.get(k) for k in ('state', 'assignment_version', 'remote_revision', 'last_applied_revision', 'last_published_revision', 'skipped', 'conflicts', 'review')}
            raise AssertionError('%s | %s status %s | sender status %s' % (error, dst.name, json.dumps(keep)[:600], json.dumps({k: aliases(src).get(src.name, {}).get(k) for k in ('state', 'last_applied_revision', 'last_published_revision', 'skipped', 'conflicts')})[:400]))
    return len(data)


def flow(kind, ctx, target, observers, controller):
    old_id, _ = rem.identity(observers[0], target.alias)
    assert target.name in aliases(target), '%s has no config status of its own before the removal' % target.name
    edit(observers[0], [observers[1], target], 'before')
    notes = ['before: an edit on %s reached %s and %s' % (observers[0].name, observers[1].name, target.name)]
    if kind == 'revoke':
        rem.confirm_revoke(observers[0], old_id)
    else:
        rem.enroll_again(target, controller, 'reset')
    for o in observers:
        notes.append('%s: %s' % (o.name, rem.seen_gone(o, target, old_id)) if kind == 'revoke' else '%s: reset seen' % o.name)
    for o in observers:
        # revoke: the entry is gone; reset: the alias now belongs to the new identity, which has no assignment (disabled)
        x.wait('%s status drops the removed machine' % o.name, lambda: (aliases(o).get(target.name) or {}).get('machine_id') != old_id and (aliases(o).get(target.name) or {}).get('state') in (None, 'disabled', 'offline', 'revoked'), 120, every=6)
    edit(observers[1], [observers[0]], 'after')
    notes.append('after: observers still sync with each other')
    if kind == 'revoke':
        rem.enroll_again(target, controller, 'reset')
    rem.sign_in(target, TOKEN)
    rem.identity(observers[0], target.alias)
    # what the machine enrolled again starts with: nothing assigned, nothing inherited
    fresh = x.pbj(target, 'config', 'status').get('environments', [])
    mine = [e for e in fresh if e.get('alias') == target.name]
    assert not mine or (mine[0].get('state') == 'disabled' and not mine[0].get('assignment_version') and not mine[0].get('mode')), 'the machine enrolled again inherited an assignment: %s' % json.dumps(mine)[:300]
    notes.append('enrolled again: no inherited assignment')
    x.pbj(target, 'config', 'repository', 'credentials', 'set', ctx.repository_id, '--file', '-', input=json.dumps(
        {'transport': 'http', 'auth': 'token', 'username': 'cfguser', 'password': ctx.password, 'allow_insecure_http': True}).encode())
    # the edits the old identity had applied are still on disk: with no sync base the new identity would report them as a
    # concurrent_update conflict and keep both sides (the safe outcome), so start the new one from a clean app folder
    target.remove(x.real(target))
    ctx.assign(next(k for k, d in ctx.devices.items() if d is target))
    edit(observers[0], [target, observers[1]], 'again')
    edit(target, observers, 'from-new')
    notes.append('assigned again: edits travel both ways')
    return '; '.join(notes)


def check(kind, ident, action):
    key = 'X-41-CFG:%s:%s' % (kind, ident)
    started = time.time()
    try:
        detail = action()
        results[key] = {'status': 'PASS', 'detail': detail}
        print('PASS %s %ds %s' % (key, time.time() - started, detail), flush=True)
    except Exception as error:  # noqa: BLE001
        results[key] = {'status': 'FAIL', 'detail': (str(error) or repr(error))[:700]}
        print('FAIL %s %ds: %s' % (key, time.time() - started, results[key]['detail']), flush=True)


def fresh_context(devices):
    ctx = x.Context(devices)
    try:
        ctx.start({'.paperboat/config-sync.toml': x.SHARED.encode(), 'cfg/settings.json': b'{}\n'})
        for key in devices:
            ctx.assign(key)
    except BaseException:
        ctx.finish()
        raise
    return ctx


def main():
    global TOKEN
    argv = sys.argv[1:]
    kinds = argv[argv.index('--only') + 1].split(',') if '--only' in argv else ['revoke', 'reset']
    targets = argv[argv.index('--targets') + 1].split(',') if '--targets' in argv else ['L', 'M', 'W']
    devices = fleet.fleet(('L', 'M', 'W'))
    for key, dev in devices.items():
        dev.key = key
    TOKEN = rem.mint_api_token()
    for dev in devices.values():
        rem.sign_in(dev, TOKEN)
    for kind in kinds:
        for key in targets:
            target = devices[key]
            observers = [d for k, d in devices.items() if k != key]
            controller = next(devices[k] for k in 'LMW' if k != key)
            ctx = fresh_context(devices)
            try:
                check(kind, key, lambda: flow(kind, ctx, target, observers, controller))
            finally:
                ctx.finish()
            for dev in devices.values():  # a reset drops the device's own sign-in
                try:
                    rem.sign_in(dev, TOKEN)
                except Exception as error:  # noqa: BLE001
                    print('sign-in of %s failed: %s' % (dev.name, str(error)[:160]), flush=True)
    out = Path(__file__).parent / 'results' / ('xplat-removal-config-%s.json' % time.strftime('%Y%m%dT%H%M%SZ', time.gmtime()))
    out.write_text(json.dumps({'check': 'xplat-removal-config', 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
