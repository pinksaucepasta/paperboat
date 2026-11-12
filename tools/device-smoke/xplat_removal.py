#!/usr/bin/env python3
"""X-41: a machine of one OS is revoked or reset (or uninstalled) and the machines of the other two OSes see it (F1-09, F1-10, F2-05).

  xplat_removal.py [--only revoke,reset,uninstall] [--targets L,M,W]

Needs the three devices of one account (xplat_fleet) and every device signed in with one API token (env ORIGIN, STATE,
PW_CHROMIUM, [PW_MODULE]: a console session of the account; a token is minted and removed again), so that a device can mint the
enrollment token for another. For each TARGET and each flavour, the other two OSes are the observers:
  before    both observers exec the target and `pb ssh` it (so their managed ssh alias file names it);
  removal   revoke (an observer revokes it by exact id), reset (the target resets and enrolls again) or uninstall (the target
            removes itself);
  seen      within 120 s neither observer lists the old machine as online, `pb exec` to its name says no such machine or that it
            is offline/revoked (never a hang or a stack trace), the old Host entry leaves the observer's ssh alias file, and
            the daemons of both observers stay under 45 diagnostic events a minute (nothing retries in a tight loop);
            after a revoke the target itself says its session is no longer valid and also stays under that rate;
  again     the target is enrolled again under its name: a NEW machine id and identity key, both observers exec and `pb ssh` it
            (a fresh enrollment is trusted; nothing was left behind that they still depended on).
Results: results/xplat-removal-<stamp>.json, ids `X-41:<flavour>:<target>`.
"""
import json
import os
import re
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import xplat_fleet as fleet  # noqa: E402

results = {}
BAD = re.compile(r'panic|goroutine |Traceback|runtime error|nil pointer')
RATE = 45
HERE = Path(__file__).parent
BUILDS = {'linux': os.environ.get('XPLAT_BUILD_L', '/tmp/macx/pb-linux-arm64'), 'darwin': os.environ.get('XPLAT_BUILD_M', '/tmp/macx/pb-darwin-arm64'),
          'windows': os.environ.get('XPLAT_BUILD_W', '/tmp/macx/pb-windows-amd64.exe')}  # only the uninstall flavour reinstalls


def mint_api_token():
    env = dict(os.environ)
    done = subprocess.run(['node', str(HERE / 'mint_api_token.mjs'), 'xplat-removal-%d' % int(time.time())], env=env, capture_output=True, text=True, timeout=180)
    assert done.returncode == 0 and done.stdout.strip(), 'no API token: ' + done.stderr[-200:]
    return done.stdout.strip()


def sign_in(dev, token):
    """Every device is signed in with the account's token, so any of them can mint an enrollment token."""
    done = dev.pb('auth', 'status', '--json')
    if done.rc == 0 and '"signed_in":true' in done.text.replace(' ', ''):
        # signed in as WHOM matters: a device another lane signed in would mint this fleet's enrollment tokens for that lane's account
        status = json.loads(done.text)
        account = status.get('data', status).get('account', {}).get('id', '')
        mine = os.environ.get('XPLAT_ACCOUNT_ID')
        if mine and account != mine:
            raise AssertionError('%s is signed in as another account (%s...), not this fleet\'s' % (dev.name, account[:13]))
        return
    if dev.os == 'linux':  # a container has no keyring: the session lives in a file the config allows
        shown = json.loads(dev.pb('config', 'show', '--json').text)
        shown = shown.get('data', shown)
        dev.write(dev.home + '/.config/paperboat/config.json',
                  json.dumps({'server_url': shown['server_url'], 'auth': {'allow_file_fallback': True}}).encode())
    done = dev.pb('login', '--api-token-file', '-', input=token.encode(), timeout=180)
    assert done.rc == 0, '%s sign-in failed: %s' % (dev.name, done.errtext[-200:])


def listed(dev):
    value = json.loads(dev.pb('machine', 'list', '--json', timeout=120).text)
    return {m['alias']: m for m in value.get('data', value)['machines']}


def events(dev):
    """Lines in the daemon's diagnostic event files on the device."""
    if dev.os == 'windows':
        return dev.device.count_in_files(dev.device.runtime_dir().parent / 'state' / 'diagnostics', 'events-*.ndjson', '\n')
    root = dev.home + ('/Library/Application Support/Paperboat/state/diagnostics' if dev.os == 'darwin' else '/.local/state/paperboat/diagnostics')
    done = dev.sh('cat "%s"/events-*.ndjson 2>/dev/null | wc -l' % root)
    return int(done.text.strip() or 0)


def alias_hosts(dev):
    path = dev.home + ('\\.ssh\\paperboat_config' if dev.os == 'windows' else '/.ssh/paperboat_config')
    text = (dev.read(path) or b'').decode('utf-8', 'replace')
    return {w.split('.')[0] for line in text.splitlines() if line.strip().lower().startswith('host ') for w in line.split()[1:]}


def works(dev, target, tries=36):
    """hostname of target through dev once it answers (the row's bound: 3 minutes); the seconds waited are returned with it."""
    last, began = None, time.time()
    for _ in range(tries):
        last = dev.pb('exec', target.alias, '--', 'hostname', timeout=90)
        if last.rc == 0 and last.text.strip():
            return last.text.strip(), int(time.time() - began)
        time.sleep(5)
    doctor = dev.pb('doctor', target.alias, timeout=120).text
    failing = [line.strip()[:160] for line in doctor.splitlines() if ': fail:' in line or ': warning:' in line]
    raise AssertionError('%s cannot exec %s: %s %s | doctor: %s' % (dev.name, target.alias, last.text[-100:], last.errtext[-250:], failing))


def ssh_works(dev, target):
    # a machine enrolled again publishes its ssh host keys within a minute or two (the message says so): wait for that, no longer
    began = time.time()
    for _ in range(60):
        done = dev.pb('ssh', target.alias, '--', 'hostname', timeout=180)
        if done.rc == 0 or 'host keys' not in (done.text + done.errtext):
            break
        time.sleep(10)
    assert done.rc == 0 and done.text.strip(), '%s: pb ssh %s after %d s: %s %s' % (dev.name, target.alias, time.time() - began, done.text[-80:], done.errtext[-250:])
    return int(time.time() - began)


def confirm_revoke(by, machine_id):
    first = json.loads(by.pb('machine', 'revoke', machine_id, '--json').text)
    token = first.get('data', first)['confirmation_token']
    done = by.pb('machine', 'revoke', machine_id, '--json', '--confirm', token)
    assert done.rc == 0, 'revoke failed: ' + (done.text + done.errtext)[-200:]


def enroll_again(target, controller, via):
    """Enroll the target again under its own name with a fresh token: `reset` (a configured install) or `pair` (a fresh install)."""
    kind = 'windows' if target.os == 'windows' else 'unix'
    token_file = target.home + ('\\.pb-xplat-token' if target.os == 'windows' else '/.pb-xplat-token')
    target.write(token_file, fleet.mint_token(kind, name=target.name, controller_dev=controller).encode(), mode='600')
    if target.os == 'windows':
        import winhost
        winhost.RemotePath(target.device, token_file).enrollment_token_file()
    try:
        if via == 'reset':  # reset removes the local setup (and the token file stays for the pair that follows)
            host = target.run(['hostname']).text.strip()
            done = target.pb('reset', '--confirmation', 'RESET PAPERBOAT', '--hostname', host, '--enrollment-token-file', token_file, '--json', timeout=300)
            value = json.loads(done.text.strip().splitlines()[-1])
            assert value.get('ok'), '%s reset failed: %s' % (target.name, done.text[-300:] + done.errtext[-300:])
        done = target.pb('pair', '--enrollment-token-file', token_file, '--name', target.name, '--json', timeout=300)
        value = json.loads(done.text.strip().splitlines()[-1])
        assert value.get('ok'), '%s enroll again (%s) failed: %s' % (target.name, via, done.text[-300:] + done.errtext[-300:])
    finally:
        target.remove(token_file)


def identity(dev, name, wait=90):
    deadline, known = time.time() + wait, []
    while True:
        machines = listed(dev)
        if name in machines and machines[name]['state'] == 'online':
            return machines[name]['id'], machines[name]['public_identity_key']
        known = sorted('%s/%s' % (alias, m['state']) for alias, m in machines.items())
        if time.time() > deadline:
            raise AssertionError('%s lists no online machine %s (has %s)' % (dev.name, name, ', '.join(known)))
        time.sleep(5)


def seen_gone(observer, target, old_id):
    deadline = time.time() + 120
    problems = []
    while time.time() < deadline:
        machine = listed(observer).get(target.alias)
        done = observer.pb('exec', target.alias, '--', 'true', timeout=60)
        text = (done.text + done.errtext).strip()
        listed_out = machine is None or machine['id'] != old_id or machine['state'] != 'online'
        refused = done.rc != 0 and bool(text) and not BAD.search(text) and len(text) < 1500
        if listed_out and refused and target.alias.split('.')[0] not in alias_hosts(observer):
            return text.splitlines()[-1][:110]
        problems = ['listed' if not listed_out else '', 'exec rc %d' % done.rc if not refused else '', 'alias file' if target.alias in alias_hosts(observer) else '']
        time.sleep(5)
    raise AssertionError('%s still sees %s after 120 s (%s)' % (observer.name, target.alias, ', '.join(p for p in problems if p)))


def rate(dev, label):
    before = events(dev)
    time.sleep(60)
    per_minute = events(dev) - before
    assert per_minute <= RATE, '%s daemon wrote %d diagnostic events in 60 s (%s)' % (dev.name, per_minute, label)
    return per_minute


def flow(kind, target, observers, controller):
    old_id, old_key = identity(observers[0], target.alias)
    for observer in observers:
        works(observer, target)
        ssh_works(observer, target)
        assert target.alias.split('.')[0] in alias_hosts(observer), '%s has no ssh alias for %s before the removal' % (observer.name, target.alias)
    notes = []
    if kind == 'revoke':
        confirm_revoke(observers[0], old_id)
    elif kind == 'reset':
        enroll_again(target, controller, 'reset')
    else:
        fleet.uninstall(target)
    if kind != 'reset':
        for observer in observers:
            notes.append('%s: %s' % (observer.name, seen_gone(observer, target, old_id)))
    else:  # the old record is replaced: observers drop the old identity as soon as the new one is announced
        for observer in observers:
            assert listed(observer)[target.alias]['id'] != old_id, '%s still lists the old identity of %s' % (observer.name, target.alias)
    if kind == 'revoke':
        status = target.pb('status')
        assert 'no longer valid' in status.errtext + status.text and 'pb login' in status.errtext + status.text, \
            'the revoked machine does not say its session is no longer valid: %r' % (status.errtext + status.text)[-200:]
        notes.append('revoked target: ' + str(rate(target, 'revoked target')) + ' events/min')
    notes.append('observers: ' + ', '.join(str(rate(o, 'observer')) for o in observers) + ' events/min')
    if kind == 'revoke':
        enroll_again(target, controller, 'reset')
    elif kind == 'uninstall':  # a fresh install of the same build, then a fresh enrollment
        fleet.install(target, BUILDS[target.os])
        enroll_again(target, controller, 'pair')
    new_id, new_key = identity(observers[0], target.alias)
    assert new_id != old_id and new_key != old_key, 'the machine enrolled again kept its identity (%s)' % new_id
    reach, waits = [], []
    for observer in observers:
        reach.append(works(observer, target)[1])
        waits.append(ssh_works(observer, target))
    return '%s; new identity after enrolling again, both observers exec and pb ssh it (exec reachable %s s after the new identity was listed, pb ssh %s s later)' % (
        '; '.join(notes), '/'.join(map(str, reach)), '/'.join(map(str, waits)))


def check(name, ident, action):
    key = 'X-41:%s:%s' % (name, ident)
    started = time.time()
    try:
        detail = action()
        results[key] = {'status': 'PASS', 'detail': detail or ''}
        print('PASS %s %ds %s' % (key, time.time() - started, detail or ''), flush=True)
    except Exception as error:  # noqa: BLE001
        results[key] = {'status': 'FAIL', 'detail': str(error)[:600]}
        print('FAIL %s %ds: %s' % (key, time.time() - started, str(error)[:600]), flush=True)


def main():
    argv = sys.argv[1:]
    kinds = argv[argv.index('--only') + 1].split(',') if '--only' in argv else ['revoke', 'reset']
    targets = argv[argv.index('--targets') + 1].split(',') if '--targets' in argv else ['L', 'M', 'W']
    devices = fleet.fleet()
    token = mint_api_token()
    for dev in devices.values():
        sign_in(dev, token)
    for kind in kinds:
        for key in targets:
            target = devices[key]
            observers = [d for k, d in devices.items() if k != key]
            controller = next(d for d in (devices['L'], devices['M']) if d is not target)
            check(kind, key, lambda: flow(kind, target, observers, controller))
            for dev in devices.values():  # a reset or uninstall of a device drops its own sign-in
                try:
                    sign_in(dev, token)
                except Exception as error:  # noqa: BLE001
                    print('sign-in of %s failed: %s' % (dev.name, str(error)[:160]), flush=True)
    stamp = time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())
    out = HERE / 'results' / ('xplat-removal-%s.json' % stamp)
    out.write_text(json.dumps({'check': 'xplat-removal', 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
