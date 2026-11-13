#!/usr/bin/env python3
"""X-41 (F1-09, F1-10, F2-05): what a revoke of the target does to its tunnel, preview, open exec, a joined terminal and its ssh alias.

  xplat_removal_extras.py L,M,W      targets, one revoke each (env of xplat_removal.py: XPLAT_*, ORIGIN, STATE, PW_CHROMIUM)

The target makes a tunnel and a preview (a live origin on 18041); one observer starts a terminal on it and the other joins it, and each
observer runs a long exec. The observer revokes the target: both terminals and both execs end with a clean error within 2 minutes, the
tunnel and the preview leave both observers' lists, and the target is enrolled again so the next target starts from a whole account.
"""
import json, sys, threading, time, uuid
from pathlib import Path
sys.path.insert(0, str(Path(__file__).parent))
import pexpect
import xplat_fleet as fleet
import xplat_removal as rem
import xplat_term as xt
from xplat_team import origin_start

KEYS = sys.argv[1].split(',')
devices = fleet.fleet(('L', 'M', 'W'))
TOKEN = rem.mint_api_token()


def jl(dev, *args):
    done = dev.pb(*args, '--json', timeout=120)
    value = json.loads(done.text)
    return value.get('items', value.get('data', value)) if isinstance(value, dict) else value


def mine(dev, kind, machine_id):
    items = jl(dev, kind, 'list')
    field = 'created_by_host_id' if kind == 'tunnel' else 'owner_machine_id'
    return [i for i in items if i.get(field) == machine_id]


def flow(key):
    target = devices[key]
    obs = [d for k, d in devices.items() if k != key]
    out = []
    old_id, _ = rem.identity(obs[0], target.alias)
    tag = uuid.uuid4().hex[:5]
    stop_origin = origin_start(target, 18041)
    time.sleep(3)
    t = target.pb('tunnel', 'create', 'rx41b-t-' + tag, '--port', '18041', '--json')
    assert t.rc == 0, 'tunnel create: ' + (t.text + t.errtext)[-200:]
    p = target.pb('preview', '18041', '--background', '--json')
    assert p.rc == 0, 'preview: ' + (p.text + p.errtext)[-200:]
    for o in obs:
        tun, pre = mine(o, 'tunnel', old_id), mine(o, 'preview', old_id)
        assert tun and pre, '%s does not list the tunnel/preview of %s before the removal (%d/%d)' % (o.name, target.name, len(tun), len(pre))
    out.append('before: both observers list the tunnel (%s) and the preview (%s) of %s' % (mine(obs[0], 'tunnel', old_id)[0]['summary_code'], mine(obs[0], 'preview', old_id)[0]['state'], target.name))
    for o in obs:
        rem.works(o, target); rem.ssh_works(o, target)
    # open flows: a shared terminal started from obs[0] and joined from obs[1], plus a long exec from each observer
    name = 'rx41b-' + tag
    first = xt.Terminal(obs[0], target, ['new', '--name', name])
    first.ready()
    second = xt.Terminal(obs[1], target, ['--session', name])
    second.kind = first.kind
    cmd, want = first.echo('B4_' + tag)
    first.say(cmd)
    second.expect(want, timeout=60)
    out.append('before: terminal %s>%s joined live from %s' % (obs[0].name, target.name, obs[1].name))
    ends = {}

    def run_exec(o):
        began = time.time()
        r = o.pb('exec', target.alias, '--', 'sleep', '900', timeout=400)
        ends[o.name] = (r.rc, int(time.time() - began), (r.text + r.errtext).strip()[-140:])
    threads = [threading.Thread(target=run_exec, args=(o,)) for o in obs]
    for th in threads:
        th.start()
    time.sleep(10)
    assert not ends, 'a long exec ended before the removal: %s' % ends
    revoked_at = time.time()
    rem.confirm_revoke(obs[0], old_id)
    try:
        for term, label in ((first, obs[0].name), (second, obs[1].name)):
            try:
                term.child.expect(pexpect.EOF, timeout=120)
            except pexpect.TIMEOUT:
                raise AssertionError('terminal on %s still open 120 s after the revoke' % label)
            out.append('terminal at %s ended %d s after the revoke: %r' % (label, time.time() - revoked_at, (term.child.before or '')[-120:].replace('\r', ' ').replace('\n', ' ')))
    finally:
        first.close(); second.close()
    for th in threads:
        th.join(timeout=150)
    assert len(ends) == 2, 'a long exec is still running 150 s after the revoke: %s' % ends
    for name_, (rc, secs, text) in ends.items():
        assert rc != 0 and secs < 200 and 'Traceback' not in text and 'panic' not in text, '%s exec after revoke: rc=%s %ss %r' % (name_, rc, secs, text)
        out.append('exec from %s ended rc=%s after %ds: %s' % (name_, rc, secs, text[-90:]))
    rem.seen_gone(obs[0], target, old_id)
    time.sleep(20)
    for o in obs:
        tun, pre = mine(o, 'tunnel', old_id), mine(o, 'preview', old_id)
        out.append('after: %s lists tunnel %s, preview %s of the revoked %s' % (o.name, [(i['desired_state'], i['summary_code']) for i in tun], [i['state'] for i in pre], target.name))
        for i in tun:
            o.pb('tunnel', 'delete', i['id'], '--json', timeout=60)
    try:
        stop_origin()
    except Exception:  # noqa: BLE001
        pass
    return '; '.join(out)


for key in KEYS:
    ident = 'X-41x:%s' % key
    began = time.time()
    try:
        detail = flow(key)
        print('PASS', ident, int(time.time() - began), detail, flush=True)
    except Exception as e:  # noqa: BLE001
        print('FAIL', ident, int(time.time() - began), str(e)[:700], flush=True)
    # enroll the target again, so the next target starts from a whole account
    try:
        ctrl = next(devices[k] for k in 'LMW' if k != key)
        rem.enroll_again(devices[key], ctrl, 'reset')
        for k in devices:
            try:
                rem.sign_in(devices[k], TOKEN)
            except Exception:  # noqa: BLE001
                pass
        print('re-enrolled', key, flush=True)
    except Exception as e:  # noqa: BLE001
        print('re-enroll failed', key, str(e)[:300], flush=True)
