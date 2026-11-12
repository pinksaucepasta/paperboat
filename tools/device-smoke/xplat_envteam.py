#!/usr/bin/env python3
"""X-12 (team part): a team ENV scope reaches machines of every OS and is withdrawn there on revoke.

  XPLAT_PREFIX=... XPLAT_TEAM=<team> XPLAT_MEMBER_CT=<container of the member's Linux machine> xplat_envteam.py [--devices L,M,W]

Needs the fleet (owner account, controller L, devices L/M/W), a paid team the owner owns with the member's account as an active
member (final_fixtures.py), the member's Linux container signed in and paired under the member account (own ENV vault; its password
in XPLAT_MEMBER_VAULT_PW, the owner's in XPLAT_VAULT_PW; both read inside, never printed). Values are random and never printed.
  deliver    every owner device is shared into the team; the owner's team value reaches a team launch on each target when the actor
             is the owner on each OTHER OS device, and when the actor is the member (granted, synced) on the member's Linux machine
  revoke     `pb env team revoke --remove MEMBER`: a member launch no longer sees the value on any target OS, the owner's still does
Results: results/xplat-envteam-<stamp>.json (`X-12:team-deliver:W<L`, `X-12:team-revoke:W`, ...).
"""
import json
import os
import sys
import time
import uuid
from itertools import permutations
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import xplat_envscopes as scopes  # noqa: E402
import xplat_fleet as fleet  # noqa: E402

results = {}
IDS = {}
TAG = uuid.uuid4().hex[:6]
TEAM = os.environ.get('XPLAT_TEAM', '')
MEMBER_PW = Path(os.environ.get('XPLAT_MEMBER_VAULT_PW', Path.home() / '.pbmac/xa2m/vault.pw'))
NAME = 'XS_TEAM_' + TAG
VALUE = 'team-' + TAG
ARGS = ['--workspace', TEAM]


def generation(owner):
    data = json.loads(owner.pb('team', 'get', TEAM, '--json').text)
    return str(data.get('data', data)['generation'])


def team(owner, *args, timeout=180):
    done = owner.pb('team', *args, '--generation', generation(owner), '--json', timeout=timeout)
    assert done.rc == 0, 'pb team %s: %s' % (' '.join(args[:3]), (done.text + done.errtext)[-250:])


def machine_ids(owner):
    listed = json.loads(owner.pb('machine', 'list', '--json').text)
    listed = listed.get('data', listed)
    return {m['alias']: m['id'] for m in listed['machines']}


def observe_team(actor, target):
    """The XS_TEAM_ variables a team-workspace `pb exec` run by `actor` gets on `target`: (rc, {name: value}, message)."""
    import base64
    if target.os == 'windows':
        script = base64.b64encode(("Get-ChildItem env: | Where-Object { $_.Name -like 'XS_TEAM_*' } | ForEach-Object { $_.Name + '=' + $_.Value }").encode('utf-16-le')).decode()
        argv = ['powershell', '-NoProfile', '-NonInteractive', '-EncodedCommand', script]
    else:
        argv = ['python3', '-c', 'import os,json;print(json.dumps({k:v for k,v in os.environ.items() if k.startswith("XS_TEAM_")}))']
    done = actor.pb(*ARGS, 'exec', IDS.get(target.alias, target.alias), '--', *argv, timeout=180)
    text = done.text.strip()
    if target.os == 'windows':
        seen = dict(line.split('=', 1) for line in text.splitlines() if line.startswith('XS_TEAM_') and '=' in line)
    else:
        seen = json.loads(text.splitlines()[-1]) if done.rc == 0 and text else {}
    return done.rc, {k: v.strip() for k, v in seen.items()}, (done.errtext or done.text)[-200:]


def until(actor, target, want, seconds=150):
    end = time.time() + seconds
    while True:
        rc, seen, message = observe_team(actor, target)
        if rc == 0 and seen == want:
            return rc, seen, message
        if time.time() > end:
            return rc, seen, message
        time.sleep(6)


def setup(owner, member, devices, member_account):
    ids = machine_ids(owner)
    IDS.update(ids)
    pw = fleet.Path(os.environ.get('XPLAT_VAULT_PW', Path.home() / '.pbmac/xplat2-vault.pw')).read_bytes().strip()
    for dev, secret in [(d, pw) for d in devices.values()]:
        assert dev.pb('env', 'vault', 'unlock', '--password-stdin', input=secret + b'\n', timeout=120).rc == 0, '%s unlock' % dev.name
    assert member.pb('env', 'vault', 'unlock', '--password-stdin', input=MEMBER_PW.read_bytes().strip() + b'\n', timeout=120).rc == 0, 'member unlock'
    for dev in devices.values():
        done = owner.pb('team', 'machine', 'share', TEAM, ids[dev.alias], '--generation', generation(owner), '--json', timeout=180)
        assert done.rc == 0 or 'already' in (done.text + done.errtext).lower(), 'share %s: %s' % (dev.name, (done.text + done.errtext)[-200:])
        done = owner.pb('team', 'machine', 'grant', TEAM, ids[dev.alias], '--member', member_account, '--capability', 'exec', '--generation', generation(owner), '--json', timeout=180)
        assert done.rc == 0, 'machine grant %s: %s' % (dev.name, (done.text + done.errtext)[-200:])
    state = json.loads(owner.pb('team', 'get', TEAM, '--json').text)
    state = state.get('data', state).get('env_status')
    if state == 'not_initialized':
        done = owner.pb('env', 'team', 'create', TEAM, '--json', timeout=180)
    else:
        done = owner.pb('env', 'team', 'reset', TEAM, '--confirm', 'RESET ENV TEAM ' + TEAM, '--json', timeout=180)
    assert done.rc == 0, 'team scope %s: %s' % ('create' if state == 'not_initialized' else 'reset', (done.text + done.errtext)[-200:])
    done = owner.pb(*ARGS, 'env', 'set', NAME, '--team', TEAM, '--value-stdin', input=VALUE.encode(), timeout=180)
    assert done.rc == 0, 'team set: ' + (done.text + done.errtext)[-200:]
    done = owner.pb('team', 'grant', TEAM, member_account, 'env', TEAM, 'read', '--generation', generation(owner), '--json', timeout=180)
    assert done.rc == 0, 'env read grant: ' + (done.text + done.errtext)[-200:]
    done = owner.pb('env', 'team', 'grant', TEAM, member_account, '--json', timeout=180)
    assert done.rc == 0, 'env team grant: ' + (done.text + done.errtext)[-200:]
    done = member.pb('env', 'grants', 'sync', '--json', timeout=180)
    assert done.rc == 0, 'member sync: ' + (done.text + done.errtext)[-200:]


def t_deliver_owner(actor, target):
    rc, seen, message = until(actor, target, {NAME: VALUE})
    assert seen == {NAME: VALUE}, 'actor %s on %s: rc=%s saw %s %s' % (actor.name, target.name, rc, sorted(seen), message)
    return 'the team value reaches %s for a team launch run by the owner on %s' % (target.name, actor.name)


def t_deliver_member(member, target):
    rc, seen, message = until(member, target, {NAME: VALUE})
    assert seen == {NAME: VALUE}, 'member on %s: rc=%s saw %s %s' % (target.name, rc, sorted(seen), message)
    return 'the team value reaches %s for the granted member (Linux machine of another account)' % target.name


def t_revoke(owner, member, target, member_account):
    rc, seen, message = observe_team(member, target)
    assert not seen, 'after revoke the member still gets %s on %s' % (sorted(seen), target.name)
    rc2, seen2, message2 = observe_team(owner, target)
    assert seen2 == {NAME: VALUE}, 'revoking the member took the value from the owner on %s: %s' % (target.name, sorted(seen2))
    return 'after revoke the member gets nothing on %s (rc=%s: %s); the owner still gets the value' % (target.name, rc, message.strip().splitlines()[-1][:80] if message.strip() else 'no value')


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
    assert TEAM, 'set XPLAT_TEAM'
    argv = sys.argv[1:]
    keys = argv[argv.index('--devices') + 1].split(',') if '--devices' in argv else ['L', 'M', 'W']
    devices = {k: d for k, d in fleet.fleet().items() if k in keys}
    owner = fleet.controller()
    controller_host, fleet.L_HOST = fleet.L_HOST, os.environ.get('XPLAT_MEMBER_HOST', fleet.L_HOST)  # the member's container may live on another host
    try:
        member = fleet.linux(name=os.environ.get('XPLAT_MEMBER_NAME', fleet.PREFIX + '-l2'), ct=os.environ['XPLAT_MEMBER_CT'])
    finally:
        fleet.L_HOST = controller_host
    member_account = json.loads(member.pb('auth', 'status', '--json').text)['account']['id']
    cleanup = []
    try:
        setup(owner, member, devices, member_account)
        cleanup.append('machines')
        for (a_key, actor), (b_key, target) in permutations(devices.items(), 2):
            check('team-deliver', '%s>%s' % (a_key, b_key), lambda: t_deliver_owner(actor, target))
        for key, target in devices.items():
            check('team-deliver', 'member>%s' % key, lambda: t_deliver_member(member, target))
        done = owner.pb('env', 'team', 'revoke', TEAM, '--remove', member_account, '--confirm', 'ROTATE ENV TEAM ' + TEAM, '--json', timeout=180)
        assert done.rc == 0, 'revoke: ' + (done.text + done.errtext)[-250:]
        member.pb('env', 'grants', 'sync', '--json', timeout=180)
        for key, target in devices.items():
            check('team-revoke', key, lambda: t_revoke(owner, member, target, member_account))
    finally:
        ids = machine_ids(owner)
        for dev in devices.values():
            owner.pb('team', 'machine', 'unshare', TEAM, ids[dev.alias], '--generation', generation(owner), '--json', timeout=180)
        owner.pb('env', 'team', 'reset', TEAM, '--confirm', 'RESET ENV TEAM ' + TEAM, '--json', timeout=180)
    stamp = time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())
    out = Path(__file__).parent / 'results' / ('xplat-envteam-%s.json' % stamp)
    out.write_text(json.dumps({'check': 'xplat-envteam', 'devices': keys, 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
