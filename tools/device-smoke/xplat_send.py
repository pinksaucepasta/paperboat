#!/usr/bin/env python3
"""X-14 and X-15: `pb send` between OSes — file names the receiver's OS cannot hold, folder trees, receiver policy.

  xplat_send.py [--devices L,M,W] [--only names,tree,inbox-set]

Each name is sent alone. It passes when the send is refused with a message that names the file or the reason, or when the
bytes arrive (sha256) under the name the sender used; a name that arrives under another name without the send saying so,
a hang, a silent overwrite and a path escape fail. The receiver's Inbox files made by the run are removed again.
Results: tools/device-smoke/results/xplat-send-<stamp>.json (`X-14:names:L>W`, `X-15:tree:L>W`, ...).
"""
import hashlib
import json
import os
import sys
import time
import unicodedata
from itertools import permutations
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import xplat_fleet as fleet  # noqa: E402

results = {}
REASONS = ('reserved', 'invalid', 'not allowed', 'cannot', 'unsupported', 'unsafe', 'forbidden', 'illegal', 'refuse')

LIST = ('import os,sys,json,hashlib\n'
        'root=sys.argv[1]\nout={}\n'
        'for d,_,fs in os.walk(root if os.name!="nt" else "\\\\\\\\?\\\\"+root):\n'
        '  for f in fs:\n'
        '    p=os.path.join(d,f)\n'
        '    rel=os.path.relpath(p,root if os.name!="nt" else "\\\\\\\\?\\\\"+root)\n'
        '    try: out[rel]=hashlib.sha256(open(p,"rb").read()).hexdigest()\n'
        '    except Exception as e: out[rel]="ERR "+str(e)[:40]\n'
        'dirs=[]\n'
        'for d,ds,_ in os.walk(root if os.name!="nt" else "\\\\\\\\?\\\\"+root):\n'
        '  for x in ds: dirs.append(os.path.relpath(os.path.join(d,x),root if os.name!="nt" else "\\\\\\\\?\\\\"+root))\n'
        'print(json.dumps({"files":out,"dirs":sorted(dirs)}))\n')

REMOVE = ('import os,sys,shutil\n'
          'root=sys.argv[1]\n'
          'pre="\\\\\\\\?\\\\" if os.name=="nt" else ""\n'
          'for rel in sys.argv[2:]:\n'
          '  p=pre+os.path.join(root,rel)\n'
          '  if os.path.isdir(p): shutil.rmtree(p, ignore_errors=True)\n'
          '  elif os.path.lexists(p): os.remove(p)\n')


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
        ok = (name[:12] in message or any(r in message.lower() for r in REASONS)) and 'could not finish the command' not in message
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


TESTS = [('names', t_names)]


def check(name, pair, action):
    key = 'X-14:%s:%s' % (name, pair)
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
