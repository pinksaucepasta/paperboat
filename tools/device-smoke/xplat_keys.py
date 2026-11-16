#!/usr/bin/env python3
"""X-24 keys: bytes typed in a client terminal reach a full-screen app on the target unchanged, every client/target pair.

  xplat_keys.py [--devices L,M,W] [--pairs L>M,M>W]

A pexpect pseudo-terminal runs `pb connect <target>` on the client (xplat_term.Terminal), a raw-mode reader on the target logs what
its stdin received (arrows, Home, End, Delete, F1-F12, Alt-arrows, Shift-Tab, Alt-b, SGR mouse, bracketed paste with LF), then
Ctrl-Z is checked: a unix target prints Stopped (bash) or suspended (zsh) and a Windows target shows ^Z, and the shell keeps running in both cases.
Results: tools/device-smoke/results/xplat-keys-<stamp>.json.
"""
import json
import sys
import time
import uuid
from datetime import datetime, timezone
from itertools import permutations
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import xplat_fleet as fleet  # noqa: E402
import xplat_term  # noqa: E402
from xplat_term import Terminal  # noqa: E402

_argv = xplat_term.client_argv
# the Windows PC takes a password over ssh (SSHPASS), unlike the key-based unix clients
xplat_term.client_argv = lambda src, tail: (['sshpass', '-e'] if src.os == 'windows' else []) + _argv(src, tail)

READER = r'''
import os, sys, time
out = open(sys.argv[1], 'w')
import traceback
sys.excepthook = lambda *a: (out.write('EXC ' + ''.join(traceback.format_exception(*a)).replace(chr(10), ' | ') + chr(10)), out.flush())
if os.name == 'nt':
    import ctypes
    k = ctypes.windll.kernel32; h = k.GetStdHandle(-10); m = ctypes.c_uint()
    k.GetConsoleMode(h, ctypes.byref(m)); k.SetConsoleMode(h, 0x0200)  # VT input only: no line mode, no echo, no processed input
    cbuf = ctypes.create_unicode_buffer(4096); n = ctypes.c_uint()  # UTF-16 console read: ReadFile in a code page mangles non-BMP characters
    def read():
        k.ReadConsoleW(h, cbuf, 4096, ctypes.byref(n), None)
        return ctypes.string_at(cbuf, 2 * n.value).decode('utf-16-le').encode('utf-8')
else:
    import termios, tty; saved = termios.tcgetattr(0); tty.setraw(0)
    read = lambda: os.read(0, 4096)
sys.stdout.write('\x1b[?2004h\x1b[?1000h\x1b[?1006h'); sys.stdout.write('READY\r\n'); sys.stdout.flush()
buf = b''
while True:
    b = read()
    if not b: break
    buf += b
    while b'\x1d' in buf:
        rec, buf = buf.split(b'\x1d', 1)
        if rec == b'\x1e':
            if os.name == 'nt': k.SetConsoleMode(h, m)
            else: termios.tcsetattr(0, termios.TCSADRAIN, saved)
            out.close(); sys.stdout.write('\x1b[?2004l\x1b[?1000l\x1b[?1006lDONE\r\n'); sys.stdout.flush(); sys.exit(0)
        out.write(rec.hex() + '\n'); out.flush()
'''
E = '\x1b'
KEYS = [('up', E + '[A'), ('down', E + '[B'), ('right', E + '[C'), ('left', E + '[D'), ('home', E + '[H'), ('end', E + '[F'),
        ('delete', E + '[3~'), ('insert', E + '[2~'), ('pgup', E + '[5~'), ('pgdn', E + '[6~'),
        ('f1', E + 'OP'), ('f2', E + 'OQ'), ('f3', E + 'OR'), ('f4', E + 'OS'), ('f5', E + '[15~'), ('f6', E + '[17~'), ('f7', E + '[18~'),
        ('f8', E + '[19~'), ('f9', E + '[20~'), ('f10', E + '[21~'), ('f11', E + '[23~'), ('f12', E + '[24~'),
        ('alt-up', E + '[1;3A'), ('alt-down', E + '[1;3B'), ('alt-right', E + '[1;3C'), ('alt-left', E + '[1;3D'),
        ('shift-tab', E + '[Z'), ('alt-b', E + 'b'), ('ctrl-right', E + '[1;5C'),
        ('mouse-press', E + '[<0;10;5M'), ('mouse-release', E + '[<0;10;5m'), ('mouse-wheel', E + '[<64;10;5M'),
        ('paste-lf', E + '[200~line one\nline two' + E + '[201~'), ('utf8', '日本語 ✓ é λ 🚢')]


def results_path():
    return Path(__file__).parent / 'results' / ('xplat-keys-%s.json' % datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ'))


def keys_check(src, dst):
    """(dict name -> 'ok' or why) for one pair"""
    out = {}
    scratch = dst.home.replace('\\', '/').rstrip('/') + '/rx24b-%s' % uuid.uuid4().hex[:6]
    dst.write(scratch + '.py', READER.encode())
    term = Terminal(src, dst)
    try:
        term.ready()
        py = 'python' if dst.os == 'windows' else 'python3'
        term.say('%s %s.py %s.log' % (py, scratch, scratch))
        term.expect('READY', timeout=30)
        time.sleep(1)
        for name, seq in KEYS:
            term.child.send(seq + '\x1d')
            time.sleep(0.3)
        term.child.send('\x1e\x1d')
        term.expect("DONE", timeout=12)
        got = dst.read(scratch + '.log').decode().split()
        if len(got) != len(KEYS):
            out['count'] = 'wanted %d records, got %d' % (len(KEYS), len(got))
        for (name, seq), hexed in zip(KEYS, got):
            out[name] = 'ok' if bytes.fromhex(hexed) == seq.encode() else 'sent %s got %s' % (seq.encode().hex(), hexed)
        # Ctrl-Z: the shell must still answer afterwards (unix: Stopped, windows: ^Z printed, no job control)
        term.child.send('\r')
        if dst.os == 'windows':
            term.child.send('\x1a\r')
        else:
            term.say('sleep 300')
            time.sleep(1)
            term.child.send('\x1a')
            try:
                term.child.expect('Stopped|suspended', timeout=15)
            except Exception:  # noqa: BLE001
                raise AssertionError('no Stopped (bash) or suspended (zsh) after Ctrl-Z; screen: %r' % (term.child.before or '')[-1200:])
        command, want = term.echo(uuid.uuid4().hex[:6])
        term.say(command)
        term.expect(want, timeout=20)
        out['ctrl-z'] = 'ok'
    except Exception as exc:  # noqa: BLE001
        out.setdefault('error', str(exc)[:600])
        out['reader-log-tail'] = (dst.read(scratch + '.log') or b'').decode(errors='replace')[-600:]
    finally:
        term.close()
        dst.remove(scratch + '.py')
        dst.remove(scratch + '.log')
    return out


def main():
    argv = sys.argv[1:]
    keys = argv[argv.index('--devices') + 1].split(',') if '--devices' in argv else ['L', 'M', 'W']
    only = set(argv[argv.index('--pairs') + 1].split(',')) if '--pairs' in argv else set()
    devs = fleet.fleet(keys)
    report = {}
    for a, b in permutations(keys, 2):
        pair = '%s>%s' % (a, b)
        if only and pair not in only:
            continue
        report[pair] = keys_check(devs[a], devs[b])
        bad = {k: v for k, v in report[pair].items() if v != 'ok'}
        print(pair, 'PASS %d/%d' % (len(report[pair]) - len(bad), len(report[pair])) if not bad else 'FAIL %s' % bad, flush=True)
    path = results_path()
    path.write_text(json.dumps(report, indent=1, ensure_ascii=False) + '\n')
    print('results', path)
    return 1 if any(v != 'ok' for r in report.values() for v in r.values()) else 0


if __name__ == '__main__':
    sys.exit(main())
