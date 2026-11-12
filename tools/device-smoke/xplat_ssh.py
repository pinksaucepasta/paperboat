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
    user = dst.run(['whoami']).text.strip().split('\\')[-1].lower()  # the OS user the target's pb runs as
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


MAKE = (
    'import os,random,sys\n'
    'root=sys.argv[1]; big=int(sys.argv[2]); r=random.Random(7)\n'
    'os.makedirs(os.path.join(root,"empty dir"),exist_ok=True)\n'
    'names=["a/b/c/deep.bin","a/one.txt","dir \u00fcn\u00ef/file 1.txt","dir \u00fcn\u00ef/\u8239.bin","top.dat"]+["many/f%03d.bin"%i for i in range(40)]\n'
    'for n in names:\n'
    '    p=os.path.join(root,*n.split("/")); os.makedirs(os.path.dirname(p),exist_ok=True)\n'
    '    open(p,"wb").write(r.randbytes(r.randrange(1,90000)))\n'
    'open(os.path.join(root,"big.bin"),"wb").write(r.randbytes(big<<20))\n')
HASH = (
    'import hashlib,json,os,sys\n'
    'root=sys.argv[1]; out={}; dirs=[]\n'
    'for d,ds,fs in os.walk(root):\n'
    '    dirs+= [os.path.relpath(os.path.join(d,x),root).replace(os.sep,"/") for x in ds]\n'
    '    for f in fs: out[os.path.relpath(os.path.join(d,f),root).replace(os.sep,"/")]=hashlib.sha256(open(os.path.join(d,f),"rb").read()).hexdigest()\n'
    'print(json.dumps({"files":out,"dirs":sorted(dirs)},sort_keys=True))\n')


def py(dev):
    return 'python' if dev.os == 'windows' else 'python3'


def tree_of(dev, root):
    done = dev.run([py(dev), '-c', HASH, root], timeout=300)
    assert done.rc == 0, done.errtext[-200:]
    return json.loads(done.text)


def make_tree(dev, root, big):
    dev.remove(root)
    done = dev.run([py(dev), '-c', MAKE, root, str(big)], timeout=300)
    assert done.rc == 0, 'tree not made on %s: %s' % (dev.name, done.errtext[-200:])
    return tree_of(dev, root)


def t_tree(src, dst):
    """F5-03/X-18: a tree (nested, empty and unicode-named directories, 46 files, a 12 MiB file) goes through scp -r, sftp put -r and
    rsync -a and arrives with the same sha256 for every file and the same directories."""
    src_root, big = native(src, 'tree'), 12
    want = make_tree(src, src_root, big)
    sep = '\\' if dst.os == 'windows' else '/'
    notes = []
    for tool in ('scp', 'sftp', 'rsync'):
        if tool == 'rsync' and 'windows' in (src.os, dst.os):
            continue
        base = native(dst, 'tree-' + tool)
        dst.remove(base)
        dst.run([py(dst), '-c', 'import os,sys;os.makedirs(sys.argv[1],exist_ok=True)', base])
        remote = slash(dst, 'tree-' + tool)
        if tool == 'scp':
            done = src.pb('scp', '-r', src_root, '%s:%s/' % (dst.alias, remote), timeout=900)
            landed = base + sep + 'tree'
        elif tool == 'sftp':
            batch = native(src, 'tree-batch.txt')
            src.write(batch, ('put -r "%s" "xplat-ssh/tree-sftp/tree"\n' % src_root.replace('\\', '/')).encode())
            done = src.pb('sftp', '-b', batch, dst.alias, timeout=900)
            landed = base + sep + 'tree'
        else:
            done = src.pb('rsync', '-a', src_root.replace('\\', '/') + '/', '%s:%s/' % (dst.alias, remote), timeout=900)
            landed = base
        assert done.rc == 0, '%s tree: %s' % (tool, (done.text + done.errtext)[-700:])
        got = tree_of(dst, landed)
        assert got == want, '%s tree differs: %d vs %d files, dirs %s vs %s' % (tool, len(got['files']), len(want['files']), got['dirs'][:3], want['dirs'][:3])
        notes.append(tool)
    return '46 files, nested/empty/unicode directories and a %d MiB file equal (sha256) through %s' % (big, ', '.join(notes))


KILLER = ('import os,signal,subprocess,sys\n'
          'p = subprocess.Popen(sys.argv[2:], **({} if os.name == "nt" else {"start_new_session": True}))\n'
          'try: p.wait(timeout=float(sys.argv[1]))\n'
          'except subprocess.TimeoutExpired:\n'
          '    # the whole client tree dies, as with a dropped connection or Ctrl-C: pb sftp/scp start the system sftp/scp as children\n'
          '    if os.name == "nt": subprocess.run(["taskkill", "/T", "/F", "/PID", str(p.pid)], capture_output=True)\n'
          '    else: os.killpg(p.pid, signal.SIGKILL)\n'
          '    p.wait(); print("KILLED")\n')


def t_resume(src, dst):
    """F5-03: a transfer cut mid-way (the client is killed after a few seconds) finishes identically when run again; rsync and
    sftp reput continue from what arrived, scp starts over, and the file is equal either way."""
    big = 64
    src_root = native(src, 'resume')
    want = make_tree(src, src_root, big)['files']['big.bin']
    notes = []
    for tool in ('scp', 'sftp', 'rsync'):
        if tool == 'rsync' and 'windows' in (src.os, dst.os):
            continue
        base = native(dst, 'resume-' + tool)
        dst.remove(base)
        dst.run([py(dst), '-c', 'import os,sys;os.makedirs(sys.argv[1],exist_ok=True)', base])
        remote = slash(dst, 'resume-' + tool)
        local = src_root.replace('\\', '/') + '/big.bin'
        if tool == 'scp':
            cmd = first = [src.pb_path, 'scp', local, '%s:%s/big.bin' % (dst.alias, remote)]
        elif tool == 'rsync':
            cmd = first = [src.pb_path, 'rsync', '-a', '--partial', '--inplace', local, '%s:%s/big.bin' % (dst.alias, remote)]
        else:
            put, reput = native(src, 'resume-put.txt'), native(src, 'resume-reput.txt')
            src.write(put, ('put "%s" "xplat-ssh/resume-sftp/big.bin"\n' % local).encode())
            src.write(reput, ('reput "%s" "xplat-ssh/resume-sftp/big.bin"\n' % local).encode())  # reput continues a partial file
            first, cmd = [src.pb_path, 'sftp', '-b', put, dst.alias], [src.pb_path, 'sftp', '-b', reput, dst.alias]
        cut = src.run([py(src), '-c', KILLER, '3', *first], timeout=300)
        assert 'KILLED' in cut.text, '%s finished within 3 s: the cut did not happen (raise the size)' % tool
        arrived = dst.run([py(dst), '-c', 'import os,sys;p=sys.argv[1];print(os.path.getsize(p) if os.path.exists(p) else 0)', base + ('\\' if dst.os == 'windows' else '/') + 'big.bin']).text.strip()
        done = src.run(cmd, timeout=900)
        assert done.rc == 0, '%s rerun after the cut: %s' % (tool, (done.text + done.errtext)[-700:])
        final = dst.sha256(base + ('\\' if dst.os == 'windows' else '/') + 'big.bin')
        assert final == want, '%s: the file after the rerun differs (arrived %s bytes before it)' % (tool, arrived)
        notes.append('%s (%s of %d MiB had arrived at the cut)' % (tool, arrived, big))
    return 'cut mid-transfer, rerun finished identically: ' + '; '.join(notes)


def t_scp_legacy(src, dst):
    """`scp -O` (the legacy protocol) works against each target next to the default SFTP mode."""
    data = os.urandom(size(src, 300 * 1024))
    local = native(src, 'legacy.bin')
    src.write(local, data)
    done = src.pb('scp', '-O', local, '%s:%s' % (dst.alias, slash(dst, 'legacy.bin')), timeout=240)
    assert done.rc == 0, 'scp -O: %s' % (done.text + done.errtext)[-300:]
    assert dst.sha256(native(dst, 'legacy.bin')) == sha(data), 'bytes differ after scp -O'
    return 'scp -O (legacy protocol) delivered %d bytes, sha256 equal' % len(data)


def t_rsync_flags(src, dst):
    """rsync flags that differ between rsync 3.x and macOS openrsync: each either works with equal bytes or fails naming rsync."""
    if 'windows' in (src.os, dst.os):
        return 'n/a: no rsync on Windows'
    data = os.urandom(200 * 1024)
    tree = native(src, 'rsf')
    src.write(tree + '/f.bin', data)
    notes = []
    for flag in ('--info=progress2', '--protect-args', '--partial'):
        dst.remove(native(dst, 'rsf-in'))
        done = src.pb('rsync', '-a', flag, tree + '/', '%s:%s/' % (dst.alias, slash(dst, 'rsf-in')), timeout=240)
        text = (done.text + done.errtext).lower()
        if done.rc == 0:
            assert dst.sha256(native(dst, 'rsf-in', 'f.bin')) == sha(data), flag + ': bytes differ'
            notes.append(flag + ' ok')
        else:
            assert 'rsync' in text and any(w in text for w in ('unrecognized', 'unknown', 'not supported', 'invalid', 'protocol', 'option')), flag + ' failed unclearly: ' + text[-250:]
            notes.append(flag + ' refused clearly')
    return '; '.join(notes) + ' (target: %s)' % dst.sh('rsync --version | head -1').text.strip()


def t_doctor(src, dst):
    """Host identity and key custody: `pb ssh doctor` for the target is healthy on this client."""
    done = src.pb('ssh', 'doctor', dst.alias, '--json', timeout=120)
    assert done.rc == 0, 'ssh doctor: %s' % (done.text + done.errtext)[-300:]
    return 'pb ssh doctor %s is healthy' % dst.alias


def t_self(src, dst):
    """`pb ssh` to the machine you are on works like ssh localhost (decision 2026-11-12): its own sshd, a command runs."""
    done = src.pb('ssh', src.alias, '--', 'echo', 'x', timeout=90)
    assert done.rc == 0 and done.text.strip() == 'x', 'pb ssh to the machine itself: rc %d %r %s' % (done.rc, done.text[-80:], done.errtext[-200:])
    return 'pb ssh to the machine itself ran a command'


TESTS = [('ssh', t_ssh), ('user', t_user), ('scp', t_scp), ('scp-backslash', t_scp_backslash), ('sftp', t_sftp), ('rsync', t_rsync), ('tree', t_tree), ('resume', t_resume), ('scp-legacy', t_scp_legacy), ('rsync-flags', t_rsync_flags), ('doctor', t_doctor)]


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
    stamp = time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())
    out = Path(__file__).parent / 'results' / ('xplat-ssh-%s.json' % stamp)

    def save():  # written after every pair, so a finished pair can be ticked while the next one runs
        out.write_text(json.dumps({'check': 'xplat-ssh', 'devices': keys, 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))

    for a_key, b_key in permutations(devices, 2):
        for name, test in TESTS:
            if only and name not in only:
                continue
            check(name, '%s>%s' % (a_key, b_key), lambda: test(devices[a_key], devices[b_key]))
        save()
    if not only or 'self' in only:
        for key, dev in devices.items():
            check('self', key, lambda: t_self(dev, dev))
    save()
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
