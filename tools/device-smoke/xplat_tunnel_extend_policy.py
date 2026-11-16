#!/usr/bin/env python3
"""C304 `pb tunnel extend` and C305 `pb tunnel policy list` on each OS of the fleet.

  xplat_tunnel_extend_policy.py [--devices L,M,W] [--only extend,policy]

extend: a durable tunnel to an origin on the device is created with a short --duration, reached, left to expire (the public URL
stops serving the marker), extended with `pb tunnel extend NAME --duration 1h` and reached again; `tunnel show` must report an
expiry about an hour away. policy: `pb tunnel policy allow <this machine> <port> --expires 90s` returns a policy ID and
generation, `policy list` shows that ID with that generation, and after the policy expired it is re-approved by ID
(`allow ... --id ID --generation G --expires 1h`): same ID, a higher generation, an expiry about an hour away; it is then revoked.
Results: tools/device-smoke/results/xplat-tunnel-extend-policy-<stamp>.json (`C304:L`, `C305:M`...). Needs a client at or after
6ff07ee50 on the device and server .47 (GET /v1/lazy-policies). Everything created is deleted/revoked on every exit path.
"""
import json
import os
import sys
import threading
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import xplat_fleet as fleet  # noqa: E402
import xplat_tunnel as xt  # noqa: E402

results = {}
TTL = os.environ.get('EXTEND_TTL', '2m')


def unwrap(text):
    value = json.loads(text)
    return value.get('data', value) if isinstance(value, dict) else value


def find(value, *keys):
    """First value of any of `keys` anywhere in a JSON document."""
    if isinstance(value, dict):
        for key in keys:
            if value.get(key) not in (None, ''):
                return value[key]
        for item in value.values():
            found = find(item, *keys)
            if found is not None:
                return found
    elif isinstance(value, list):
        for item in value:
            found = find(item, *keys)
            if found is not None:
                return found
    return None


def minutes_from_now(stamp):
    when = datetime.fromisoformat(str(stamp).replace('Z', '+00:00'))
    return (when - datetime.now(timezone.utc)).total_seconds() / 60


def pbj(dev, *args, timeout=120):
    done = dev.pb(*args, '--json', timeout=timeout)
    assert done.rc == 0, 'pb %s -> rc %d: %s' % (' '.join(args[:3]), done.rc, (done.text + done.errtext)[-300:])
    return unwrap(done.text)


def wait(what, check, seconds, every=8):
    end = time.time() + seconds
    while time.time() < end:
        if check():
            return time.time()
        time.sleep(every)
    raise AssertionError('timed out waiting for ' + what)


def serving(url, marker):
    out = xt.curl(url + '/', '--http1.1', timeout=20).stdout.decode(errors='replace').rsplit('\n', 1)
    return out[0] == marker and out[-1].split()[0] == '200'


def t_extend(dev):
    port = 33000 + sum(map(ord, dev.name)) % 2000
    marker, name = 'c304-' + dev.os, 'c304-' + uuid.uuid4().hex[:8]
    threading.Thread(target=lambda: dev.run([xt.py(dev), '-c', xt.ORIGIN, '127.0.0.1', str(port), marker], timeout=900), daemon=True).start()
    assert dev.run([xt.py(dev), '-c', xt.WAIT_ORIGIN, '127.0.0.1', str(port)], timeout=180).rc == 0, 'origin never answered'
    created = pbj(dev, 'tunnel', 'create', name, '--port', str(port), '--duration', TTL, '--wait', timeout=240)
    try:
        url = xt.endpoint(created)
        assert serving(url, marker), 'new tunnel does not serve the marker'
        first = find(created, 'expires_at')
        wait('the tunnel to expire (public URL stops serving)', lambda: not serving(url, marker), 420, every=10)
        gone = xt.curl(url + '/', '--http1.1', timeout=20).stdout.decode(errors='replace').replace('\n', ' ')[:80]
        done = dev.pb('tunnel', 'extend', name, '--duration', '1h', '--json', timeout=120)
        assert done.rc == 0, 'extend failed: ' + (done.text + done.errtext)[-300:]
        stamp = find(unwrap(done.text), 'expires_at')
        assert stamp and 50 < minutes_from_now(stamp) < 70, 'extend result expiry %r is not about 1h away' % stamp
        back = wait('the extended tunnel to serve again', lambda: serving(url, marker), 180, every=6)
        shown = find(pbj(dev, 'tunnel', 'show', name), 'expires_at')
        assert shown and 50 < minutes_from_now(shown) < 70, 'tunnel show expiry %r is not about 1h away' % shown
        human = dev.pb('tunnel', 'extend', name, '--duration', '2h', timeout=120)
        assert human.rc == 0 and 'now expires' in human.text, 'human extend output: ' + human.text[-200:]
        refused = dev.pb('tunnel', 'extend', name, timeout=60)
        assert refused.rc != 0, 'extend without --duration was accepted'
        xt.curl(url + '/__stop', '--http1.1')
        return 'created with --duration %s (expiry %s); expired -> %r; extend 1h -> expiry %s, serving again, show %s; human output ok; no-duration refused' % (TTL, first, gone, stamp, shown)
    finally:
        xt.confirm_delete(dev, name)


def t_policy(dev):
    port = 34000 + sum(map(ord, dev.name)) % 2000
    policy = None
    try:
        made = pbj(dev, 'tunnel', 'policy', 'allow', dev.name, str(port), '--expires', '90s')
        policy, generation = find(made, 'id', 'policy_id'), find(made, 'generation')
        assert policy and str(policy).startswith('lap_') and generation, 'allow result lacks ID/generation: %r' % made
        listed = dev.pb('tunnel', 'policy', 'list', timeout=60)
        assert listed.rc == 0 and policy in listed.text and 'generation %s' % generation in listed.text, 'policy list text: %r' % listed.text[-300:]
        data = pbj(dev, 'tunnel', 'policy', 'list')
        rows = data if isinstance(data, list) else (find(data, 'items', 'policies') or [])
        row = next((r for r in rows if find(r, 'id', 'policy_id') == policy), None)
        assert row and str(find(row, 'generation')) == str(generation), 'policy list --json lacks %s generation %s: %r' % (policy, generation, data)
        time.sleep(100)  # the 90 s policy lifetime plus the server's clock skew
        again = pbj(dev, 'tunnel', 'policy', 'allow', dev.name, str(port), '--id', policy, '--generation', str(generation), '--expires', '1h')
        new_gen, stamp = find(again, 'generation'), find(again, 'expires_at', 'expires')
        assert find(again, 'id', 'policy_id') == policy and int(new_gen) > int(generation), 're-approval did not keep the ID / raise the generation: %r' % again
        assert stamp and 50 < minutes_from_now(stamp) < 70, 're-approved expiry %r is not about 1h away' % stamp
        got = pbj(dev, 'tunnel', 'policy', 'get', policy)
        assert int(find(got, 'generation')) == int(new_gen), 'get shows another generation: %r' % got
        after = dev.pb('tunnel', 'policy', 'list', timeout=60)
        assert policy in after.text and 'generation %s' % new_gen in after.text, 'list after re-approval: %r' % after.text[-300:]
        return 'allow -> %s generation %s; list shows both (text and json); expired, re-approved by ID -> generation %s expiring %s; get matches' % (policy, generation, new_gen, stamp)
    finally:
        if policy:
            done = dev.pb('tunnel', 'policy', 'revoke', policy, '--json', timeout=60)
            try:
                token = find(unwrap(done.text), 'confirmation_token')
            except ValueError:
                token = None
            if token:
                dev.pb('tunnel', 'policy', 'revoke', policy, '--confirm', token, '--json', timeout=60)


def check(row, key, action):
    name = '%s:%s' % (row, key)
    started = time.time()
    try:
        detail = action()
        results[name] = {'status': 'PASS', 'detail': detail}
        print('PASS %s %ds %s' % (name, time.time() - started, detail[:400]), flush=True)
    except Exception as error:  # noqa: BLE001
        results[name] = {'status': 'FAIL', 'detail': str(error)[:900]}
        print('FAIL %s %ds: %s' % (name, time.time() - started, str(error)[:900]), flush=True)


def selftest():
    doc = {'data': {'tunnel': {'id': 't1', 'expires_at': '2999-01-01T00:00:00Z'}, 'items': [{'policy_id': 'lap_1', 'generation': 2}]}}
    assert unwrap(json.dumps(doc))['tunnel']['id'] == 't1' and find(unwrap(json.dumps(doc)), 'expires_at').startswith('2999')
    assert find(doc, 'items')[0]['policy_id'] == 'lap_1' and find(doc, 'nope') is None
    assert minutes_from_now('2999-01-01T00:00:00Z') > 1e6 and minutes_from_now('2000-01-01T00:00:00+00:00') < 0
    print('selftest ok')


def main():
    argv = sys.argv[1:]
    if '--selftest' in argv:
        return selftest()
    keys = argv[argv.index('--devices') + 1].split(',') if '--devices' in argv else ['L', 'M', 'W']
    only = argv[argv.index('--only') + 1].split(',') if '--only' in argv else ['extend', 'policy']
    devices = {k: d for k, d in fleet.fleet().items() if k in keys}
    for key, dev in devices.items():
        dev.key = key
        if 'extend' in only:
            check('C304', key, lambda: t_extend(dev))
        if 'policy' in only:
            check('C305', key, lambda: t_policy(dev))
    stamp = time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())
    out = Path(__file__).parent / 'results' / ('xplat-tunnel-extend-policy-%s.json' % stamp)
    out.write_text(json.dumps({'check': 'xplat-tunnel-extend-policy', 'devices': keys, 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
