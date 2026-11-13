#!/usr/bin/env python3
"""X-14 and X-15: `pb send` between OSes — file names the receiver's OS cannot hold, batch limit, file modes, folder/link refusal, resume, `pb inbox set`.

  xplat_send.py [--devices L,M,W] [--only names,batch,modes,refuse,inboxset,resume]

Each name is sent alone. It passes when the send is refused with a message that names the file or the reason, or when the
bytes arrive (sha256) under the name the sender used; a name that arrives under another name without the send saying so,
a hang, a silent overwrite and a path escape fail. The receiver's Inbox files made by the run are removed again.
Results: tools/device-smoke/results/xplat-send-<stamp>.json (`X-14:names:L>W`, `X-15:batch:L>W`, ...).
"""
import hashlib
import json
import os
import sys
import threading
import time
import unicodedata
from itertools import permutations
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import xplat_fleet as fleet  # noqa: E402

results = {}
REASONS = ('reserved', 'invalid', 'not allowed', 'cannot', 'unsupported', 'unsafe', 'forbidden', 'illegal', 'refuse')

PREFIX = r"""
import os
def P(r):
    if os.name != "nt":
        return r
    return "\\\\?\\UNC\\" + r[2:] if r.startswith("\\\\") else "\\\\?\\" + r
"""
LIST = PREFIX + r"""
import sys, json, hashlib
root = sys.argv[1]
base = P(root)
out, dirs = {}, []
for d, ds, fs in os.walk(base):
    for f in fs:
        p = os.path.join(d, f)
        try:
            out[os.path.relpath(p, base)] = hashlib.sha256(open(p, "rb").read()).hexdigest()
        except Exception as e:
            out[os.path.relpath(p, base)] = "ERR " + str(e)[:40]
    for x in ds:
        dirs.append(os.path.relpath(os.path.join(d, x), base))
print(json.dumps({"files": out, "dirs": sorted(dirs)}))
"""

REMOVE = PREFIX + r"""
import sys, shutil
root = sys.argv[1]
for rel in sys.argv[2:]:
    p = os.path.join(P(root), rel)
    if os.path.isdir(p):
        shutil.rmtree(p, ignore_errors=True)
    elif os.path.lexists(p):
        os.remove(p)
"""


def py(dev):
    return 'python' if dev.os == 'windows' else 'python3'


def inbox_path(dev):
    return json.loads(dev.pb('inbox', 'path', '--json').text)['data']['path']


def inbox_listing(dev):
    done = dev.run([py(dev), '-c', LIST, inbox_path(dev)], timeout=120)
    assert done.rc == 0, done.errtext[-300:]
    return json.loads(done.text)


def inbox_clean(dev, before):
    now = inbox_listing(dev)
    extra = [n for n in now['files'] if n not in before['files']] + [d for d in now['dirs'] if d not in before['dirs']]
    top = sorted({n.replace('\\', '/').split('/')[0] for n in extra})
    if top:
        dev.run([py(dev), '-c', REMOVE, inbox_path(dev), *top], timeout=120)


def unix_names():
    return ['a:b.txt', 'trail.', 'sp ', 'CON.txt', 'q"u*?.txt', 'A.txt', 'a.txt', 'x' * 250 + '.txt',
            unicodedata.normalize('NFC', 'café-nfc.txt'), unicodedata.normalize('NFD', 'café-nfd.txt'), '-lead.txt',
            'sp ace and üñí.txt']


def common_names():
    return ['A.txt', 'a.txt', 'x' * 250 + '.txt', unicodedata.normalize('NFC', 'café-nfc.txt'),
            unicodedata.normalize('NFD', 'café-nfd.txt'), '-lead.txt', 'sp ace and üñí.txt']


def sender_dir(src):
    return src.home + ('\\xplat-send\\out' if src.os == 'windows' else '/xplat-send/out')


def one_name(src, dst, name, content, before):
    path = sender_dir(src) + ('\\' if src.os == 'windows' else '/') + name
    src.write(path, content)
    done = src.pb('send', path, '--to', dst.alias, timeout=120)  # the human output reports where each file landed
    message = (done.text + done.errtext).strip()
    if done.rc != 0:
        ok = name[:12].replace('"', '\\"') in message and any(r in message.lower() for r in REASONS) and 'could not finish the command' not in message  # X-14: names the file AND the reason
        return ('refused', ok, message[-200:])
    stored = [n for n, h in inbox_listing(dst)['files'].items() if h == hashlib.sha256(content).hexdigest() and n not in before['files']]
    if not stored:
        return ('lost', False, 'the send succeeded but no file with these bytes is in the receiver Inbox: ' + message[-150:])
    same = name in stored or unicodedata.normalize('NFC', name) in [unicodedata.normalize('NFC', s) for s in stored]
    reported = any(s in message for s in stored)
    return ('arrived', same or reported, 'stored as %r' % stored[0][:60] + ('' if same else ' (renamed%s)' % (', reported' if reported else ', NOT reported')))


def t_names(src, dst):
    names = common_names() if src.os == 'windows' else unix_names()
    before = inbox_listing(dst)
    seen, bad = [], []
    try:
        for index, name in enumerate(names):
            content = ('xplat names %d %r\n' % (index, name)).encode() + os.urandom(64)
            started = time.time()
            try:
                kind, ok, detail = one_name(src, dst, name, content, inbox_listing(dst))
            except Exception as error:  # noqa: BLE001 a hang or a transport error is a failure of that name
                kind, ok, detail = 'error', False, str(error)[:160]
            seen.append('%r: %s (%s)' % (name[:20], kind, detail[:90]))
            if not ok:
                bad.append('%r %s %s' % (name[:30], kind, detail[:200]))
            if time.time() - started > 100:
                bad.append('%r took %ds' % (name[:30], time.time() - started))
    finally:
        inbox_clean(dst, before)
    assert not bad, '; '.join(bad)
    return '; '.join(seen)


def sep(dev):
    return '\\' if dev.os == 'windows' else '/'


def sent_file(src, name, data, mode=None):
    path = sender_dir(src) + sep(src) + name
    src.write(path, data, mode=mode)
    return path


def landed(dst, before, data):
    """Inbox entries that appeared since `before` and hold exactly these bytes."""
    digest = hashlib.sha256(data).hexdigest()
    return [n for n, h in inbox_listing(dst)['files'].items() if h == digest and n not in before['files']]


def t_batch(src, dst):
    """The 10-file batch limit: ten files in one send arrive with equal bytes, eleven are refused naming the limit."""
    before = inbox_listing(dst)
    try:
        blobs = [('xplat-b%02d.bin' % i, os.urandom(2048)) for i in range(11)]
        paths = [sent_file(src, n, d) for n, d in blobs]
        done = src.pb('send', *paths[:10], '--to', dst.alias, timeout=300)
        assert done.rc == 0, 'ten files were not sent: ' + (done.text + done.errtext)[-200:]
        missing = [n for n, d in blobs[:10] if not landed(dst, before, d)]
        assert not missing, 'files missing or different in the Inbox: %s' % missing
        mid = inbox_listing(dst)
        done = src.pb('send', *paths, '--to', dst.alias, timeout=120)
        message = done.text + done.errtext
        assert done.rc != 0 and '10' in message and 'could not finish' not in message, 'eleven files: rc=%s %s' % (done.rc, message[-200:])
        assert inbox_listing(dst)['files'].keys() == mid['files'].keys(), 'a refused batch still delivered files'
        return 'ten files arrive (sha256 equal); eleven are refused: %s' % message.strip().splitlines()[-1][:120]
    finally:
        inbox_clean(dst, before)


def t_modes(src, dst):
    """Received files follow the receiver's policy: no setuid/setgid, no executable bit, owner-only; the sender's mode is not copied."""
    before = inbox_listing(dst)
    sent = []
    try:
        if src.os == 'windows':
            variants = [('tool.exe', None), ('plain.txt', None)]
        else:
            variants = [('suid.bin', '4755'), ('exec.sh', '0755'), ('wide.txt', '0777'), ('private.txt', '0600')]
        for name, mode in variants:
            data = ('mode %s\n' % name).encode() + os.urandom(48)
            done = src.pb('send', sent_file(src, name, data, mode), '--to', dst.alias, timeout=120)
            assert done.rc == 0, '%s not sent: %s' % (name, (done.text + done.errtext)[-160:])
            found = landed(dst, before, data)
            assert found, '%s did not arrive' % name
            sent.append((name, found[0]))
        root = inbox_path(dst)
        if dst.os == 'windows':
            for name, stored in sent:
                script = ("$a = (Get-Acl -LiteralPath ('%s\\' + '%s')).Access | Where-Object { $_.IdentityReference.Value -match 'Everyone|BUILTIN\\\\Users|Authenticated Users' }; "
                          "if ($a) { 'OPEN' } else { 'OWNER' }" % (root, stored.replace("'", "''")))
                out = dst.sh(script).text.strip()
                assert out.endswith('OWNER'), '%s on Windows is readable by other accounts' % name
            return 'files arrive owner-only on %s (no Everyone/Users entry)' % dst.name
        bad = []
        for name, stored in sent:
            mode = int(dst.sh('stat -c %%a -- "%s/%s" 2>/dev/null || stat -f %%Lp -- "%s/%s"' % (root, stored, root, stored)).text.strip() or '0', 8)
            if mode & 0o7177:
                bad.append('%s mode %o' % (name, mode))
        assert not bad, 'the receiver kept sender permission bits: ' + ', '.join(bad)
        return 'sender modes (4755, 0755, 0777, 0600) all arrive without setuid, group/other or exec bits'
    finally:
        inbox_clean(dst, before)


def t_refuse(src, dst):
    """A folder and a symbolic link are refused the same way on every OS: the message names the path and what to do."""
    before = inbox_listing(dst)
    base = sender_dir(src)
    folder, target, link = [base + sep(src) + n for n in ('xplat-folder', 'xplat-real.txt', 'xplat-link.txt')]
    seen = []
    try:
        src.write(folder + sep(src) + 'inside.txt', b'inside')
        src.write(target, b'target')
        if src.os == 'windows':
            src.sh("Remove-Item -Force -ErrorAction SilentlyContinue '%s'; New-Item -ItemType SymbolicLink -Path '%s' -Target '%s' | Out-Null" % (link, link, target))
        else:
            src.sh('rm -f "%s"; ln -s "%s" "%s"' % (link, target, link))
        assert src.exists(link), 'could not make a symbolic link on the sender'
        for path, word in ((folder, 'is a folder'), (link, 'is a symbolic link')):
            done = src.pb('send', path, '--to', dst.alias, timeout=120)
            message = done.text + done.errtext
            assert done.rc != 0 and path in message and word in message and 'could not finish' not in message, '%s: rc=%s %s' % (path, done.rc, message[-240:])
            seen.append(word)
        assert inbox_listing(dst)['files'].keys() == before['files'].keys(), 'a refused source delivered something'
        return 'refused with the path and what to do: %s' % ', '.join(seen)
    finally:
        inbox_clean(dst, before)
        src.remove(link)
        src.remove(folder + sep(src) + 'inside.txt')


def t_resume(src, dst):
    """A send interrupted by the receiver's daemon restarting finishes with equal bytes (the sender waits and resumes)."""
    before = inbox_listing(dst)
    data = os.urandom(60 << 20)
    try:
        path = sent_file(src, 'xplat-resume.bin', data)
        outcome = {}
        thread = threading.Thread(target=lambda: outcome.setdefault('done', src.pb('send', path, '--to', dst.alias, timeout=900)))
        thread.start()
        time.sleep(4)
        restart = dst.pb('service', 'restart', timeout=180)
        thread.join(timeout=900)
        done = outcome.get('done')
        assert done is not None and done.rc == 0, 'the send did not finish after the receiver restarted (restart rc=%s): %s' % (restart.rc, ((done.text + done.errtext)[-250:]) if done else 'hung')
        assert landed(dst, before, data), 'the file arrived with different bytes after an interrupted send'
        return 'send finished with equal bytes after %s restarted its daemon mid-transfer (restart rc=%s)' % (dst.name, restart.rc)
    finally:
        inbox_clean(dst, before)
        src.remove(sender_dir(src) + sep(src) + 'xplat-resume.bin')


def inbox_set(dev, typed):
    return dev.pb('inbox', 'set', typed, timeout=120)


def t_inboxset(src, dst):
    """`pb inbox set` takes this computer's own path form: ~, a path with spaces and non-ASCII, (Windows) another drive and a share;
    a relative path or the other OS's form is refused naming it, the Inbox path unchanged, and a send lands in the new folder."""
    original = inbox_path(dst)
    other_form = '/home/xplat/in' if dst.os == 'windows' else 'C:\\Users\\xplat\\In'
    relative = 'xplat\\rel' if dst.os == 'windows' else 'xplat/rel'
    home = dst.home
    accepted = [home + sep(dst) + 'xplat inbox üñí']
    if dst.os == 'windows':
        accepted += ['D:\\xplat-inbox', '\\\\localhost\\c$\\xplat-share-inbox']
    try:
        for typed in (relative, other_form):
            done = inbox_set(dst, typed)
            message = done.text + done.errtext
            assert done.rc != 0 and typed in message and 'is not a full path' in message and 'could not finish' not in message, '%r: rc=%s %s' % (typed, done.rc, message[-200:])
            assert inbox_path(dst) == original, 'a refused path changed the Inbox'
        done = inbox_set(dst, '~/xplat tilde')
        assert done.rc == 0 and '~' not in inbox_path(dst) and inbox_path(dst).startswith(home), '~ was not expanded: %s' % (done.text + done.errtext)[-150:]
        for path in accepted:
            done = inbox_set(dst, path)
            assert done.rc == 0 and inbox_path(dst) == path, '%s refused: %s' % (path, (done.text + done.errtext)[-200:])
            data = os.urandom(512)
            before = inbox_listing(dst)
            sent = src.pb('send', sent_file(src, 'xplat-set.bin', data), '--to', dst.alias, timeout=180)
            assert sent.rc == 0 and landed(dst, before, data), 'a send did not land in %s: %s' % (path, (sent.text + sent.errtext)[-160:])
            inbox_clean(dst, before)
        return 'refused: relative and %s form; accepted: ~, spaces/non-ASCII%s; each delivered' % ('Linux' if dst.os == 'windows' else 'Windows', ', other drive, share' if dst.os == 'windows' else '')
    finally:
        dst.pb('inbox', 'reset', timeout=120)


TESTS = [('names', t_names), ('batch', t_batch), ('modes', t_modes), ('refuse', t_refuse), ('inboxset', t_inboxset), ('resume', t_resume)]


def check(name, pair, action):
    key = '%s:%s:%s' % ('X-14' if name == 'names' else 'X-15', name, pair)
    started = time.time()
    try:
        detail = action()
        results[key] = {'status': 'PASS', 'detail': detail or ''}
        print('PASS %s %ds %s' % (key, time.time() - started, (detail or '')[:400]), flush=True)
    except Exception as error:  # noqa: BLE001
        results[key] = {'status': 'FAIL', 'detail': str(error)[:900]}
        print('FAIL %s %ds: %s' % (key, time.time() - started, str(error)[:900]), flush=True)


def main():
    argv = sys.argv[1:]
    keys = argv[argv.index('--devices') + 1].split(',') if '--devices' in argv else ['L', 'M', 'W']
    only = set(argv[argv.index('--only') + 1].split(',')) if '--only' in argv else set()
    devices = {k: d for k, d in fleet.fleet().items() if k in keys}
    for a_key, b_key in permutations(devices, 2):
        for name, test in TESTS:
            if only and name not in only:
                continue
            check(name, '%s>%s' % (a_key, b_key), lambda: test(devices[a_key], devices[b_key]))
    stamp = time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())
    out = Path(__file__).parent / 'results' / ('xplat-send-%s.json' % stamp)
    out.write_text(json.dumps({'check': 'xplat-send', 'devices': keys, 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
