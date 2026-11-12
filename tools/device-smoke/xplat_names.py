#!/usr/bin/env python3
"""X-40: machine names across OSes (F2-04, F5-04, F9-01): `pb machine rename` from one OS on a machine of another.

  xplat_names.py [--devices L,M,W] [--only refused,case,reserved,propagate]

For each ordered pair the SOURCE device renames the TARGET machine (every device of the account may rename any machine of it):
  refused    names that are not a DNS label or not a file name on every OS (space, underscore, Unicode, apostrophe, leading
             hyphen, trailing dot, empty, 64 characters) are refused with a short message and the name is unchanged;
  case       an upper-case name is stored lower-case, and a second name that differs only by case is the same name (one record);
             63 characters are accepted;
  reserved   CON, NUL and COM1 are refused or normalised, and an accepted name works on every OS (exec by that name);
  propagate  after a rename the new name resolves from the source and from the third OS (exec, `pb ssh`, completion), the old
             name is gone, and the old name given to ANOTHER machine reaches that machine (a stale alias never reaches the
             renamed one).
Every target is renamed back to its own name at the end. Results: results/xplat-names-<stamp>.json, ids `X-40:<check>:<A>><B>`.
"""
import json
import re
import sys
import time
from itertools import permutations
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import xplat_fleet as fleet  # noqa: E402

results = {}
BAD = re.compile(r'panic|goroutine |Traceback|runtime error|nil pointer')
REFUSED = ['has space', 'under_score', 'ünïcode', "alice's-macbook", '-leading', 'trailing.', 'a' * 64, 'x/y']
LABEL = re.compile(r'^[a-z0-9]([a-z0-9-]*[a-z0-9])?$')


def listing():
    return fleet.machines()


def alias_of(machine_id):
    return next((alias for alias, m in listing().items() if m['id'] == machine_id), None)


def restore(src, dst, machine_id, name):
    for _ in range(3):
        if alias_of(machine_id) == name:
            return
        src.pb('machine', 'rename', machine_id, name, '--json', timeout=90)
        time.sleep(2)
    raise AssertionError('could not restore %s to %s (now %s)' % (machine_id, name, alias_of(machine_id)))


def resolves(dev, name, tries=8):
    """`pb exec <name> -- hostname` works from dev: the machine answers (the daemon's own inventory follows the server by seconds)."""
    last = None
    for _ in range(tries):
        last = dev.pb('exec', name, '--', 'hostname', timeout=90)
        if last.rc == 0 and last.text.strip():
            return last.text.strip()
        time.sleep(5)
    raise AssertionError('%s cannot exec %s: %s %s' % (dev.name, name, last.text[-100:], last.errtext[-200:]))


def gone(dev, name, tries=8):
    last = None
    for _ in range(tries):
        last = dev.pb('exec', name, '--', 'hostname', timeout=90)
        if last.rc != 0:
            return (last.errtext or last.text).strip().splitlines()[-1][:120]
        time.sleep(5)
    raise AssertionError('%s still reaches the old name %s' % (dev.name, name))


def alias_hosts(dev, expect):
    """The Host names in the device's managed ssh alias file (~/.ssh/paperboat_config), after a refresh by pb ssh."""
    path = dev.home + ('\\.ssh\\paperboat_config' if dev.os == 'windows' else '/.ssh/paperboat_config')
    hosts = set()
    for _ in range(6):
        text = (dev.read(path) or b'').decode('utf-8', 'replace')
        hosts = {w for line in text.splitlines() if line.strip().lower().startswith('host ') for w in line.split()[1:]}
        if any(h == expect or h.startswith(expect + '.') for h in hosts):
            break
        time.sleep(5)
    return {h.split('.')[0] for h in hosts}


def completes(dev, name):
    # whole candidates only: the new name contains the old one (`macx-l-r` contains `macx-l`)
    return name in [line.split('\t')[0].strip() for line in dev.pb('__complete', 'exec', '').text.splitlines()]


def t_refused(src, dst, third, mid):
    for name in REFUSED + ['']:
        done = src.pb('machine', 'rename', mid, name, '--json', timeout=90)
        text = done.text + done.errtext
        assert done.rc != 0, 'rename to %r was accepted (now %s)' % (name, alias_of(mid))
        assert text.strip() and len(text) < 1500 and not BAD.search(text), 'unclear refusal for %r: %r' % (name, text[-200:])
        assert alias_of(mid) == dst.alias, 'a refused rename to %r changed the name to %s' % (name, alias_of(mid))
    for name in REFUSED:  # at enroll: the enrollment token for such a name is refused before any machine exists
        done = src.pb('machine', 'add', '--name', name, '--json', timeout=90)
        assert done.rc != 0, 'pb machine add --name %r was accepted' % name
    return '%d invalid names refused at rename and at enroll with a message, name unchanged' % (len(REFUSED) + 1)


def t_case(src, dst, third, mid):
    for name, want in (('Mixed-Case-9', 'mixed-case-9'), ('MIXED-case-9', 'mixed-case-9'), ('b' * 63, 'b' * 63)):
        done = src.pb('machine', 'rename', mid, name, '--json', timeout=90)
        assert done.rc == 0, 'rename to %r refused: %s' % (name, (done.text + done.errtext)[-200:])
        got = alias_of(mid)
        assert got == want, 'rename to %r stored %r, want %r' % (name, got, want)
        assert LABEL.match(got) and len(got) <= 63, 'stored name %r is not a DNS label' % got
        same = [a for a, m in listing().items() if a.lower() == want]
        assert len(same) == 1, 'names that differ only by case are %d records: %s' % (len(same), same)
    return 'upper case stored lower-case, a case-only difference is the same name (one record), 63 characters accepted'


def t_reserved(src, dst, third, mid):
    notes = []
    for name in ('CON', 'nul', 'COM1'):
        done = src.pb('machine', 'rename', mid, name, '--json', timeout=90)
        if done.rc != 0:
            text = (done.text + done.errtext).strip()
            assert text and len(text) < 1500 and not BAD.search(text), 'unclear refusal for %r: %r' % (name, text[-200:])
            notes.append('%s refused' % name)
            continue
        got = alias_of(mid)
        assert got == name.lower(), '%s stored as %r' % (name, got)
        for dev in (src, third):
            resolves(dev, got)
        notes.append('%s stored as %s, exec by that name works from both other OSes' % (name, got))
    return '; '.join(notes)


def tunnel_create(dst):
    for attempt in range(3):  # the tunnel is only a fixture of this check: a transient server error is retried
        done = dst.pb('tunnel', 'create', 'x40-%s-%d' % (dst.name, int(time.time())), '--port', '9411', '--duration', '30m', '--json', timeout=120)
        if done.rc == 0:
            break
        time.sleep(15)
    assert done.rc == 0, 'tunnel create on %s: %s' % (dst.name, (done.text + done.errtext)[-300:])
    value = json.loads(done.text)
    return value.get('data', value)['tunnel']['id']


def tunnel_delete(dev, tunnel_id):
    first = json.loads(dev.pb('tunnel', 'delete', tunnel_id, '--json').text)
    dev.pb('tunnel', 'delete', tunnel_id, '--json', '--confirm', first.get('data', first)['confirmation_token'])


def t_propagate(src, dst, third, mid):
    tunnel = tunnel_create(dst)
    try:
        return propagate(src, dst, third, mid, tunnel)
    finally:
        tunnel_delete(dst, tunnel)


def propagate(src, dst, third, mid, tunnel):
    old, new = dst.alias, dst.alias + '-r'
    done = src.pb('machine', 'rename', mid, new, '--json', timeout=90)
    assert done.rc == 0, 'rename refused: ' + (done.text + done.errtext)[-200:]
    for dev in (src, third):  # a tunnel names its machine by id: it is unchanged, active and still listed after the rename
        shown = json.loads(dev.pb('tunnel', 'show', tunnel, '--json', timeout=90).text)
        shown = shown.get('data', shown).get('tunnel', shown.get('data', shown))
        assert shown['created_by_host_id'] == mid and shown['desired_state'] == 'active', '%s sees the tunnel as %s' % (dev.name, shown)
    host = dst.run(['hostname']).text.strip().lower().split('.')[0]
    for dev in (src, third):
        got = resolves(dev, new).lower().split('.')[0]
        assert got == host, '%s reached %r by the new name, want %r' % (dev.name, got, host)
        message = gone(dev, old)
        ssh = dev.pb('ssh', new, '--', 'hostname', timeout=120)
        assert ssh.rc == 0 and ssh.text.strip().lower().split('.')[0] == host, '%s: pb ssh %s: %s %s' % (dev.name, new, ssh.text[-80:], ssh.errtext[-200:])
        aliases = alias_hosts(dev, new)
        assert new in aliases and old not in aliases, '%s ssh config aliases after the rename: %s' % (dev.name, sorted(aliases))
        assert completes(dev, new) and not completes(dev, old), '%s completion: new %s old %s' % (dev.name, completes(dev, new), completes(dev, old))
    # the old name now belongs to another machine: it reaches that machine, never the renamed one
    third_id = listing()[third.alias]['id']
    try:
        assert src.pb('machine', 'rename', third_id, old, '--json', timeout=90).rc == 0, 'the freed name was refused for another machine'
        got = resolves(src, old).lower().split('.')[0]
        assert got == third.run(['hostname']).text.strip().lower().split('.')[0], 'the old name %s reached %r, not the machine that now has it' % (old, got)
        assert got != host, 'a stale alias reached the renamed machine'
    finally:
        restore(src, third, third_id, third.alias)
    return 'new name resolves from both other OSes (exec, pb ssh, completion); old name gone (%s); the freed name reaches only its new owner' % message


TESTS = [('refused', t_refused), ('case', t_case), ('reserved', t_reserved), ('propagate', t_propagate)]


def check(name, pair, action):
    key = 'X-40:%s:%s' % (name, pair)
    started = time.time()
    try:
        detail = action()
        results[key] = {'status': 'PASS', 'detail': detail or ''}
        print('PASS %s %ds %s' % (key, time.time() - started, detail or ''), flush=True)
    except Exception as error:  # noqa: BLE001
        results[key] = {'status': 'FAIL', 'detail': str(error)[:500]}
        print('FAIL %s %ds: %s' % (key, time.time() - started, str(error)[:500]), flush=True)


def main():
    argv = sys.argv[1:]
    keys = argv[argv.index('--devices') + 1].split(',') if '--devices' in argv else ['L', 'M', 'W']
    only = set(argv[argv.index('--only') + 1].split(',')) if '--only' in argv else set()
    devices = fleet.fleet()
    assert len(keys) == 3, 'the propagate check needs all three OSes'
    for a_key, b_key in permutations(keys, 2):
        src, dst = devices[a_key], devices[b_key]
        third = devices[next(k for k in keys if k not in (a_key, b_key))]
        mid = listing()[dst.alias]['id']
        try:
            for name, test in TESTS:
                if only and name not in only:
                    continue
                check(name, '%s>%s' % (a_key, b_key), lambda: test(src, dst, third, mid))
        finally:
            restore(src, dst, mid, dst.alias)
    stamp = time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())
    out = Path(__file__).parent / 'results' / ('xplat-names-%s.json' % stamp)
    out.write_text(json.dumps({'check': 'xplat-names', 'devices': keys, 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
