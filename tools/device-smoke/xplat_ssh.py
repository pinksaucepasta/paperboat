#!/usr/bin/env python3
"""X-18: the SSH family (`pb ssh`, `scp`, `sftp`, `rsync`) from each OS to each other OS, both directions.

  xplat_ssh.py [--devices L,M,W] [--only ssh,scp,sftp,rsync,self,user]

Results: tools/device-smoke/results/xplat-ssh-<stamp>.json, one entry per check and ordered pair (`X-18:scp:L>W`).
"""
import hashlib
import json
import os
import sys
import time
from itertools import permutations
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import xplat_fleet as fleet  # noqa: E402

results = {}
USER = {'linux': 'tester', 'darwin': 'admin', 'windows': 'runneradmin'}


def slash(dev, *parts):
    """A path below the device's home, in the forward-slash form scp and sftp take for every OS."""
    return dev.home.replace('\\', '/') + '/xplat-ssh/' + '/'.join(parts)


def native(dev, *parts):
    base = dev.home + ('\\xplat-ssh' if dev.os == 'windows' else '/xplat-ssh')
    return base + ''.join(('\\' if dev.os == 'windows' else '/') + p for p in parts)


def size(src, wanted):
    """A Windows controller writes files 3 KB per call: its source files stay small."""
    return min(wanted, 48 * 1024) if src.os == 'windows' else wanted


def sha(data):
    return hashlib.sha256(data).hexdigest()


def t_ssh(src, dst):
    done = src.pb('ssh', dst.alias, '--', 'hostname', timeout=120)
    ours = dst.run(['hostname']).text.strip().lower()
    assert done.rc == 0 and done.text.strip().lower().split('.')[0] == ours.split('.')[0], 'ssh hostname %r vs %r: %s' % (done.text[-80:], ours, done.errtext[-200:])
    return 'pb ssh ran a command on the target (hostname %s)' % ours


def t_user(src, dst):
    user = USER[dst.os]
    for form in (['--user', user, dst.alias], [user + '@' + dst.alias]):
        done = src.pb('ssh', *form, '--', 'whoami', timeout=120)
        assert done.rc == 0 and user in done.text.lower(), 'pb ssh %s -> rc %d %r %s' % (form, done.rc, done.text[-80:], done.errtext[-150:])
    return '--user and user@machine land as %s' % user


def t_scp(src, dst):
    data = os.urandom(size(src, 512 * 1024)) + b'\r\n\x00'
    local, back = native(src, 'out.bin'), native(src, 'back.bin')
    src.write(local, data)
    done = src.pb('scp', local, '%s:%s' % (dst.alias, slash(dst, 'in.bin')), timeout=240)
    assert done.rc == 0, 'scp to %s: %s %s' % (dst.name, done.text[-150:], done.errtext[-250:])
    assert dst.sha256(native(dst, 'in.bin')) == sha(data), 'bytes differ on the target'
    done = src.pb('scp', '%s:%s' % (dst.alias, slash(dst, 'in.bin')), back, timeout=240)
    assert done.rc == 0 and src.sha256(back) == sha(data), 'scp back: %s %s' % (done.text[-150:], done.errtext[-250:])
    return 'scp both ways sha256 equal (%d bytes), remote path in forward-slash form' % len(data)


def t_scp_backslash(src, dst):
    """A remote path with backslashes or a drive letter: accepted, or refused with a message that says what to write."""
    if dst.os != 'windows':
        return 'n/a: the target is not Windows'
    local = native(src, 'bs.bin')
    src.write(local, b'backslash\n')
    done = src.pb('scp', local, '%s:%s' % (dst.alias, native(dst, 'bs.bin')), timeout=120)
    if done.rc == 0:
        assert dst.sha256(native(dst, 'bs.bin')) == sha(b'backslash\n')
        return 'a backslash remote path was accepted'
    message = (done.text + done.errtext).strip()
    assert message and len(message) < 600 and ('path' in message.lower() or 'backslash' in message.lower() or 'drive' in message.lower()), \
        'refused without telling the user what to write: %r' % message[-300:]
    return 'refused clearly: ' + message.splitlines()[-1][:120]


def t_sftp(src, dst):
    data = os.urandom(size(src, 200 * 1024))
    local, back = native(src, 'sf.bin'), native(src, 'sf-back.bin')
    src.write(local, data)
    batch = native(src, 'batch.txt')
    remote = 'xplat-ssh/sf.bin'  # sftp paths on every target are relative to the login directory (a Windows drive path is not)
    src.write(batch, ('put "%s" "%s"\nget "%s" "%s"\n' % (local.replace('\\', '/'), remote, remote, back.replace('\\', '/'))).encode())
    done = src.pb('sftp', '-b', batch, dst.alias, timeout=240)
    assert done.rc == 0 and src.sha256(back) == sha(data) and dst.sha256(native(dst, 'sf.bin')) == sha(data), \
        'sftp: %s %s' % (done.text[-150:], done.errtext[-250:])
    return 'sftp put and get sha256 equal (%d bytes)' % len(data)


def t_rsync(src, dst):
    data = os.urandom(size(src, 300 * 1024))
    tree = native(src, 'rs')
    src.write(tree + ('\\' if src.os == 'windows' else '/') + 'f.bin', data)
    done = src.pb('rsync', '-a', tree.replace('\\', '/') + '/', '%s:%s/' % (dst.alias, slash(dst, 'rs-in')), timeout=240)
    if src.os == 'windows' or dst.os == 'windows':
        message = (done.text + done.errtext).strip()
        if done.rc == 0:
            assert dst.sha256(native(dst, 'rs-in', 'f.bin')) == sha(data)
            return 'rsync works with a Windows endpoint'
        assert 'rsync' in message.lower() and any(w in message.lower() for w in ('not found', 'not installed', 'install', 'unavailable', 'missing', 'not recognized')), \
            'rsync with a Windows endpoint failed without a clear message: %r' % message[-300:]
        return 'clear message: ' + message.splitlines()[-1][:130]
    assert done.rc == 0 and dst.sha256(native(dst, 'rs-in', 'f.bin')) == sha(data), 'rsync: %s %s' % (done.text[-150:], done.errtext[-250:])
    version = dst.sh('rsync --version | head -1').text.strip()
    return 'rsync -a sha256 equal; target: ' + version


def t_self(src, dst):
    done = src.pb('ssh', src.alias, '--', 'echo', 'x', timeout=60)
    message = (done.text + done.errtext).lower()
    assert done.rc != 0 and ('same machine' in message or 'this machine' in message or 'itself' in message or 'yourself' in message), \
        'pb ssh to the machine itself was not refused clearly: rc %d %r' % (done.rc, message[-250:])
    return 'refused: ' + message.strip().splitlines()[0][:110]


TESTS = [('ssh', t_ssh), ('user', t_user), ('scp', t_scp), ('scp-backslash', t_scp_backslash), ('sftp', t_sftp), ('rsync', t_rsync)]


def check(name, pair, action):
    key = 'X-18:%s:%s' % (name, pair)
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
    devices = {k: d for k, d in fleet.fleet().items() if k in keys}
    for key, dev in devices.items():
        dev.write(native(dev, '.keep'), b'')  # the directory every scp, sftp and rsync of this run lands in
        print(key, 'ssh doctor rc', dev.pb('ssh', 'doctor', dev.alias).rc, flush=True)
    for a_key, b_key in permutations(devices, 2):
        for name, test in TESTS:
            if only and name not in only:
                continue
            check(name, '%s>%s' % (a_key, b_key), lambda: test(devices[a_key], devices[b_key]))
    if not only or 'self' in only:
        for key, dev in devices.items():
            check('self', key, lambda: t_self(dev, dev))
    stamp = time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())
    out = Path(__file__).parent / 'results' / ('xplat-ssh-%s.json' % stamp)
    out.write_text(json.dumps({'check': 'xplat-ssh', 'devices': keys, 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
