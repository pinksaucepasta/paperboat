#!/usr/bin/env python3
"""X-36: version skew between OSes. One machine runs the previous build, the other two the build under test.

  xplat_skew.py skew   --old L|M|W --old-dir DIR --new-dir DIR [--devices L,M,W]
  xplat_skew.py bundle --new-dir DIR [--devices L,M,W]

skew:   the machine of OS `old` gets the previous build (a fresh install of the older binary); then for every other device Y the existing cross-OS checks run in both directions on the pair
        (terminal, exec, ssh, send, private access, tunnel, config sync). A check passes when it works; a failing one counts only if
        its message names the older machine and the fix (update). The new build goes back on the machine afterwards. Both swaps are
        fresh installs (uninstall, install, pair again), never a downgrade over a newer install.
bundle: `pb update bundle` made on A and `pb update --from` on B, every ordered pair of different OSes: refused with the message that
        names both OSes, the installed version unchanged and the machine still working.
Environment is that of xplat_fleet.py (XPLAT_PREFIX, XPLAT_L_CT, XPLAT_MAC_SSH, SMOKE_WINDOWS_SSH, XPLAT_CONTROLLER=L).
Results: results/xplat-skew-<stamp>.json (`X-36:<feature>:<old>:<A>><B>`, `X-36:bundle:A>B`).
"""
import json
import re
import subprocess
import sys
import time
from itertools import permutations
from pathlib import Path

HERE = Path(__file__).parent
sys.path.insert(0, str(HERE))
import xplat_commands  # noqa: E402
import xplat_fleet as fleet  # noqa: E402

results = {}
BINARIES = {'L': 'pb-linux-amd64', 'M': 'pb-darwin-arm64', 'W': 'pb-windows-amd64.exe'}
# (script, extra args): the quick cases of each feature's own cross-OS check
FEATURES = [('terminal', 'xplat_term.py', ['--only', 'shell,utf8']), ('exec', 'xplat_exec.py', ['--only', 'argv,exit']),
            ('ssh', 'xplat_ssh.py', ['--only', 'ssh,scp']), ('send', 'xplat_send.py', ['--only', 'names']),
            ('access', 'xplat_access.py', ['--core']), ('tunnel', 'xplat_tunnel.py', ['--only', 'v4']),
            ('config', 'xplat_config.py', ['--only', 'X-01'])]
NAMES_OLDER = re.compile(r'(older|update|upgrade|version)', re.I)


def check(name, key, action):
    started = time.time()
    try:
        detail = action()
        results[key] = {'status': 'PASS', 'detail': detail or ''}
        print('PASS %s %ds %s' % (key, time.time() - started, (detail or '')[:300]), flush=True)
    except Exception as error:  # noqa: BLE001
        results[key] = {'status': 'FAIL', 'detail': str(error)[:700]}
        print('FAIL %s %ds: %s' % (key, time.time() - started, str(error)[:700]), flush=True)


def version(dev):
    return dev.pb('version').text.strip().splitlines()[1].split()[-1]


def run_feature(script, args, pair):
    """The feature's own check on the two devices of `pair` (both directions); returns {check id: (status, detail)} from its result file."""
    done = subprocess.run([sys.executable, str(HERE / script), '--devices', ','.join(pair), *args], capture_output=True, text=True, timeout=3600, cwd=HERE)
    path = re.findall(r'(\S+\.json)\s*$', done.stdout.strip().splitlines()[-1] if done.stdout.strip() else '')
    assert path, '%s printed no result file: %s' % (script, (done.stdout + done.stderr)[-300:])
    data = json.loads(Path(path[0]).read_text())
    return {r['id']: (r['status'], r['detail']) for r in data['results']}, path[0]


def fresh(dev, binary):
    """A machine on exactly `binary`: uninstalled (its record revoked), installed from nothing and paired again. A downgrade over a newer
    install is not a product path (the older build meets state a newer one wrote), so the previous build is never installed over."""
    xplat_commands.uninstall(dev, xplat_commands.controller())
    if dev.os == 'windows':
        dev.sh('icacls C:\\Users\\runneradmin\\.ssh /setowner runneradmin')
    fleet.install(dev, binary)
    fleet.pair(dev, 'windows' if dev.os == 'windows' else 'unix')


def skew(old, old_dir, new_dir, keys, features=()):
    devices = {k: d for k, d in fleet.fleet().items() if k in keys}
    victim = devices[old]
    fresh(victim, Path(old_dir) / BINARIES[old])
    old_version = version(victim)
    try:
        for other in devices:
            if other == old:
                continue
            others = version(devices[other])
            for feature, script, args in FEATURES:
                if features and feature not in features:
                    continue
                key = 'X-36:%s:%s:%s' % (feature, old, other)

                def action():
                    found, path = run_feature(script, args, (old, other))
                    bad = {k: d for k, (s, d) in found.items() if s != 'PASS'}
                    unnamed = {k: d for k, d in bad.items() if not (NAMES_OLDER.search(d) and victim.name in d)}
                    assert not unnamed, 'failed without naming the older machine %s and the fix: %s' % (victim.name, '; '.join('%s: %s' % (k, d[:140]) for k, d in unnamed.items()))
                    return '%d checks on %s(%s)<->%s(%s): %d worked, %d refused naming %s and the fix (%s)' % (
                        len(found), old, old_version, other, others, len(found) - len(bad), len(bad), victim.name, Path(path).name)
                check(feature, key, action)
    finally:
        fresh(victim, Path(new_dir) / BINARIES[old])


def bundle(new_dir, keys):
    devices = {k: d for k, d in fleet.fleet().items() if k in keys}
    out = {}
    for key, dev in devices.items():
        path = dev.home + ('\\xe-%s.pb-update' % key if dev.os == 'windows' else '/xe-%s.pb-update' % key)
        done = dev.pb('update', 'bundle', '--output', path, '--json', timeout=600)
        assert done.rc == 0, 'pb update bundle on %s: %s' % (dev.name, (done.text + done.errtext)[-300:])
        out[key] = path
    names = {'L': 'linux', 'M': 'darwin', 'W': 'windows'}
    for a_key, b_key in permutations(devices, 2):
        src, dst = devices[a_key], devices[b_key]

        def action():
            local = Path('/tmp') / ('xe-bundle-%s.pb-update' % a_key)
            data = src.read(out[a_key])
            assert data, 'no bundle on ' + src.name
            local.write_bytes(data)
            remote = dst.home + ('\\xe-in-%s.pb-update' % a_key if dst.os == 'windows' else '/xe-in-%s.pb-update' % a_key)
            dst.write(remote, data)
            before = version(dst)
            done = dst.pb('update', '--from', remote, '--json', timeout=600)
            text = done.text + done.errtext
            assert done.rc != 0, 'a bundle made on %s was accepted on %s' % (names[a_key], names[b_key])
            assert names[a_key] in text and names[b_key] in text and 'pb update bundle' in text, 'refusal does not name both OSes and the fix: ' + text[-300:]
            assert version(dst) == before, 'the installed version changed from %s to %s' % (before, version(dst))
            ok = dst.pb('exec', src.alias, '--', 'hostname', timeout=120)
            assert ok.rc == 0, 'the machine stopped working after the refusal: ' + (ok.text + ok.errtext)[-200:]
            dst.remove(remote)
            return 'refused on %s with the message naming %s and %s; version %s unchanged, exec still works' % (dst.name, names[a_key], names[b_key], before)
        check('bundle', 'X-36:bundle:%s>%s' % (a_key, b_key), action)


def main():
    argv = sys.argv[1:]
    mode = argv[0]
    keys = argv[argv.index('--devices') + 1].split(',') if '--devices' in argv else ['L', 'M', 'W']
    option = lambda name: argv[argv.index(name) + 1]  # noqa: E731
    if mode == 'skew':
        skew(option('--old'), option('--old-dir'), option('--new-dir'), keys, option('--features').split(',') if '--features' in argv else ())
    else:
        bundle(option('--new-dir'), keys)
    out = HERE / 'results' / ('xplat-skew-%s.json' % time.strftime('%Y%m%dT%H%M%SZ', time.gmtime()))
    out.write_text(json.dumps({'check': 'xplat-skew-' + mode, 'devices': keys, 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
