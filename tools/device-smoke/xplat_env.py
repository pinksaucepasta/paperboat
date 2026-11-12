#!/usr/bin/env python3
"""X-11: ENV delivered and used identically on each OS — `pb exec`, `pb ssh` command sessions and `pb env run`.

  XPLAT_PREFIX=lanex2 XPLAT_L_CT=laneX2-l XPLAT_CONTROLLER=L SMOKE_WINDOWS_SSH=winpool-N xplat_env.py [--devices L,M,W]

Runs against the fleet of a FRESH account whose ENV vault is unlocked with the password in ~/.pbmac/xplat2-vault.pw (read
inside this script, never printed). One set of variables (digits and underscores in names; values with spaces, quotes, `$`, `%`,
backslash, emoji, several lines, 4 KiB; a PATH-like value) is set once; each target's own Python prints the sha256 of every
value it sees, and that is compared with the sha256 of what was set. Results: tools/device-smoke/results/xplat-env-<stamp>.json.
"""
import base64
import hashlib
import json
import os
import shlex
import sys
import time
from itertools import permutations
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import xplat_fleet as fleet  # noqa: E402

results = {}
PASSWORD = Path(os.environ.get('XPLAT_VAULT_PW', Path.home() / '.pbmac/xplat2-vault.pw'))
VALUES = {
    'XP_1_PLAIN': 'plain',
    'XP_2_SPACES': '  leading and  inner   spaces and trailing  ',
    'XP_3_QUOTES': '"double" \'single\' `tick`',
    'XP_4_DOLLAR': '$HOME ${PATH} $(whoami) $$',
    'XP_5_PERCENT': '%PATH% %USERPROFILE% 100%',
    'XP_6_BACKSLASH': 'C:\\Users\\x\\ trailing\\ \\\\unc\\share\\',
    'XP_7_EMOJI': 'emoji \U0001F680 wide \u8239 accent \u00fc\u00f1 nfd e\u0301',
    'XP_8_MULTI': 'line one\nline two\n\nline four with tab\there',
    'XP_9_LONG': ('0123456789abcdef' * 256),
    'XP_A_PATHLIKE_UNIX': '/usr/bin:/opt/x/bin:~/bin',
    'XP_B_PATHLIKE_WIN': 'C:\\Windows;C:\\Program Files\\x;D:\\tools',
}
DUMP = ('import os,sys,json,hashlib\n'
        'print(json.dumps({k:hashlib.sha256(os.environ[k].encode("utf-8","surrogatepass")).hexdigest() for k in sorted(os.environ) if k.startswith("XP_")}))\n')


def sha(value):
    return hashlib.sha256(value.encode()).hexdigest()


def py(dev):
    return 'python' if dev.os == 'windows' else 'python3'


def expected():
    return {k: sha(v) for k, v in sorted(VALUES.items())}


def compare(got, where):
    want = expected()
    missing = sorted(set(want) - set(got))
    changed = sorted(k for k in want if k in got and got[k] != want[k])
    assert not missing and not changed, '%s: missing %s changed %s' % (where, missing, changed)


def parse(text):
    return json.loads(text.strip().splitlines()[-1])


def setup(devices):
    password = PASSWORD.read_bytes().strip()
    for dev in devices.values():
        done = dev.pb('env', 'vault', 'unlock', '--password-stdin', input=password + b'\n', timeout=120)
        assert done.rc == 0, '%s: unlock failed: %s' % (dev.name, (done.text + done.errtext)[-200:])
    for dev in devices.values():  # the startup-file hook is how a plain command session (pb ssh) receives ENV
        shell = 'powershell' if dev.os == 'windows' else dev.sh('basename "$SHELL"').text.strip() or 'bash'  # the user's own login shell
        done = dev.pb('env', 'hook', shell, '--install', timeout=120)
        assert done.rc == 0, '%s: hook install failed: %s' % (dev.name, (done.text + done.errtext)[-200:])
    ctl = fleet.controller()
    for name, value in VALUES.items():
        done = ctl.pb('env', 'set', name, '--value-stdin', input=value.encode(), timeout=120)
        assert done.rc == 0, 'set %s: %s' % (name, (done.text + done.errtext)[-200:])


def host_state(dev):
    """The machine's published and observed ENV delivery from `pb env host show` (metadata only)."""
    done = dev.pb('env', 'host', 'show', '--machine', dev.alias, '--json', timeout=120)
    data = json.loads(done.text)
    data = data.get('data', data)
    return {'published': data.get('published'), 'applied': data.get('applied'), 'layers': len(data.get('layers', []))}


def wait_applied(dev, seconds=120):
    """Delivery is observed after the host applies it; a host that has not yet reports applied=false, so this waits a bounded
    time and returns the state either way: the byte comparison below is the check."""
    end = time.time() + seconds
    state = host_state(dev)
    while time.time() < end and not (state['published'] and state['applied']):
        time.sleep(8)
        state = host_state(dev)
    assert state['published'], '%s: nothing published for this host: %s' % (dev.name, state)
    return json.dumps(state)


def t_exec(src, dst):
    done = src.pb('exec', dst.alias, '--', py(dst), '-c', DUMP, timeout=180)
    assert done.rc == 0, done.errtext[-300:] + done.text[-200:]
    compare(parse(done.text), 'pb exec on ' + dst.name)
    return 'pb exec: %d values equal (sha256) on %s' % (len(VALUES), dst.name)


def ssh_command(dst):
    """The command line `pb ssh` runs on the target: Python reading the dump script from stdin through the target's own shell
    (PowerShell loads its profile, the hook; cmd.exe has no hook and is not the shell under test)."""
    if dst.os == 'windows':
        script = "@'\n" + DUMP + "'@ | python -"
        return ['powershell', '-EncodedCommand', base64.b64encode(script.encode('utf-16-le')).decode()]
    return [shlex.join([py(dst), '-c', DUMP.replace('\n', ';').replace('print(', 'print(', 1)])]


def t_ssh(src, dst):
    done = src.pb('ssh', dst.alias, '--', *ssh_command(dst), timeout=180)
    assert done.rc == 0, done.errtext[-300:] + done.text[-200:]
    compare(parse(done.text), 'pb ssh on ' + dst.name)
    return 'pb ssh command session: %d values equal on %s' % (len(VALUES), dst.name)


def t_run(src, dst):
    if src is not dst:
        return 'n/a'
    done = src.pb('env', 'run', '--', py(src), '-c', DUMP, timeout=180)
    assert done.rc == 0, done.errtext[-300:] + done.text[-200:]
    compare(parse(done.text), 'pb env run on ' + src.name)
    return 'pb env run: %d values equal on %s' % (len(VALUES), src.name)


TESTS = [('exec', t_exec), ('ssh', t_ssh)]


def check(name, pair, action):
    key = 'X-11:%s:%s' % (name, pair)
    started = time.time()
    try:
        detail = action()
        results[key] = {'status': 'PASS', 'detail': detail or ''}
        print('PASS %s %ds %s' % (key, time.time() - started, (detail or '')[:300]), flush=True)
    except Exception as error:  # noqa: BLE001
        results[key] = {'status': 'FAIL', 'detail': str(error)[:700]}
        print('FAIL %s %ds: %s' % (key, time.time() - started, str(error)[:700]), flush=True)


def main():
    argv = sys.argv[1:]
    keys = argv[argv.index('--devices') + 1].split(',') if '--devices' in argv else ['L', 'M', 'W']
    devices = {k: d for k, d in fleet.fleet().items() if k in keys}
    setup(devices)
    try:
        for key, dev in devices.items():
            check('applied', key, lambda: wait_applied(dev)[:120])
        for key, dev in devices.items():
            check('run', key, lambda: t_run(dev, dev))
        for a_key, b_key in permutations(devices, 2):
            for name, test in TESTS:
                check(name, '%s>%s' % (a_key, b_key), lambda: test(devices[a_key], devices[b_key]))
    finally:
        for dev in devices.values():
            dev.pb('env', 'hook', '--uninstall', timeout=120)
    stamp = time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())
    out = Path(__file__).parent / 'results' / ('xplat-env-%s.json' % stamp)
    out.write_text(json.dumps({'check': 'xplat-env', 'devices': keys, 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
