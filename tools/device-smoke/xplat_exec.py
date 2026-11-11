#!/usr/bin/env python3
"""X-17 (and the exec leg of X-11): `pb exec` from each OS to each other OS, both directions.

  xplat_exec.py [--devices L,M,W] [--only argv,stdin,...]

The oracle on every target is its own Python (python3 on L and M, python on W): it prints what it received, so what is
compared is the target's view of argv, stdin, cwd, environment and exit status. Results go to
tools/device-smoke/results/xplat-exec-<stamp>.json with one entry per check and ordered pair (`X-17:argv:L>W`).
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
DUMP = 'import sys,json;sys.stdout.write(json.dumps(sys.argv[1:]))'
ARGV = ['a b', '', 'q"uote', 'back\\', 'two\\\\', 'sp ace\\', "it's", '$HOME', '%PATH%', '*', 'ünï€', 'tab\there', '-x', '--', 'a;b&c|d']


def python(dev):
    return 'python' if dev.os == 'windows' else 'python3'


def target_home(dev):
    return dev.home


def run(src, dst, args, flags=(), input=None, timeout=120):
    return src.pb('exec', dst.alias, *flags, '--', *args, input=input, timeout=timeout)


def t_argv(src, dst):
    done = run(src, dst, [python(dst), '-c', DUMP, *ARGV])
    assert done.rc == 0, done.errtext[-300:] + done.text[-200:]
    got = json.loads(done.text)
    assert got == ARGV, 'argv changed on the way: %r' % (got,)
    return '%d arguments exact (spaces, empty, quotes, trailing backslashes, unicode, shell metacharacters)' % len(ARGV)


def t_cmd(src, dst):
    if dst.os != 'windows':
        done = run(src, dst, ['sh', '-c', 'printf "%s|" "$0" "$@"', 'one two', 'x"y'])
        assert done.text == 'one two|x"y|', done.text
        return 'sh -c runs exactly as given'
    done = run(src, dst, ['cmd', '/c', 'echo a b & echo c'])
    assert done.rc == 0 and [x.strip() for x in done.text.splitlines()] == ['a b', 'c'], done.text
    done = run(src, dst, ['powershell', '-NoProfile', '-Command', "Write-Output 'a b'; Write-Output 'c'"])
    assert done.rc == 0 and done.text.split() == ['a', 'b', 'c'], done.text
    return 'cmd /c "<one command line>" and powershell -Command run as given'


def t_stdin(src, dst):
    size = 60 * 1024 if src.os == 'windows' else 900 * 1024  # the Windows controller stages stdin 3 KB at a time
    data = os.urandom(size) + b'\r\n\xef\xbb\xbf\r\n\x00\xff' * 64
    code = 'import sys,hashlib;sys.stdout.write(hashlib.sha256(sys.stdin.buffer.read()).hexdigest())'
    done = run(src, dst, [python(dst), '-c', code], input=data)
    assert done.rc == 0 and done.text.strip() == hashlib.sha256(data).hexdigest(), done.errtext[-200:] + done.text[-100:]
    return 'stdin sha256 equal for %d bytes (binary, CRLF, BOM, NUL)' % len(data)


def t_cwd(src, dst):
    good = dst.home
    other = '/tmp' if dst.os == 'windows' else 'C:\\Windows\\Temp'
    done = run(src, dst, [python(dst), '-c', 'import os;print(os.getcwd())'], flags=['--cwd', good])
    assert done.rc == 0 and done.text.strip().lower() == good.lower(), 'cwd %s -> %r %s' % (good, done.text, done.errtext[-150:])
    done = run(src, dst, [python(dst), '-c', 'print(1)'], flags=['--cwd', other])
    message = done.errtext + done.text
    assert done.rc != 0 and '--cwd must be an absolute path on the target machine' in message, \
        'a working directory of the other OS form %s: rc %d %r' % (other, done.rc, message[:200])
    return '--cwd %s accepted, %s refused: %s' % (good, other, message.strip().splitlines()[0][:110])


def t_cwd_outside(src, dst):
    outside = 'C:\\Windows\\Temp' if dst.os == 'windows' else '/tmp'
    done = run(src, dst, [python(dst), '-c', 'print(1)'], flags=['--cwd', outside])
    message = done.errtext + done.text
    assert done.rc != 0 and 'working directory' in message.lower() and 'workspace' in message.lower(), \
        'a directory outside the machine\'s workspace root was refused without saying so: rc %d %r' % (done.rc, message[:200])
    return 'refused with: ' + message.strip().splitlines()[0][:110]


def t_env_streams(src, dst):
    code = ('import os,sys;sys.stdout.write(os.environ.get("XP_A","").encode("utf-8").hex()+"|"+os.environ.get("XP_B","").encode("utf-8").hex());'
            'sys.stderr.write("err-text");sys.exit(0)')
    a, b = 'with space "q" $x %y%', 'ünï€'
    done = run(src, dst, [python(dst), '-c', code], flags=['--env', 'XP_A=' + a, '--env', 'XP_B=' + b])
    assert done.rc == 0, done.errtext
    assert done.text == a.encode().hex() + '|' + b.encode().hex(), done.text
    assert done.errtext.strip() == 'err-text', 'stderr was not kept apart: %r / %r' % (done.errtext, done.text)
    return '--env arrives byte for byte; stdout and stderr stay separate'


def t_exit(src, dst):
    for code in (0, 1, 7, 255):
        done = run(src, dst, [python(dst), '-c', 'import sys;sys.exit(%d)' % code])
        assert done.rc == code, 'exit %d came back as %d' % (code, done.rc)
    if dst.os == 'windows':
        # a Unix process cannot exit above 255, so the code is read from the JSON events (a Windows client shows it as is)
        done = run(src, dst, ['python', '-c', 'import ctypes;ctypes.windll.kernel32.ExitProcess(3221225477)'], flags=['--json'])
        events = [json.loads(line) for line in done.text.splitlines() if line.startswith('{')]
        code = next(e['result']['code'] for e in events if e.get('event') == 'exited')
        assert code == 3221225477, 'a Windows exit code above 255 (0xC0000005) was reported as %d' % code
        return 'codes 0,1,7,255 and 3221225477 exact'
    done = run(src, dst, ['sh', '-c', 'kill -9 $$'])
    assert done.rc == 137, 'SIGKILL came back as %d, documented 128+9' % done.rc
    return 'codes 0,1,7,255 exact; SIGKILL = 137'


def t_pty(src, dst):
    code = 'import sys;sys.stdout.write("tty=%s" % sys.stdout.isatty())'
    done = run(src, dst, [python(dst), '-c', code], flags=['--pty'])
    assert done.rc == 0 and 'tty=True' in done.text, done.text + done.errtext
    return '--pty allocates a terminal on the target'


def t_big(src, dst):
    size = 8 * 1024 * 1024
    code = ('import sys,hashlib;b=(b"0123456789abcdef"*64+b"\\n")*(%d//1025+1);b=b[:%d];sys.stdout.buffer.write(b)' % (size, size))
    done = run(src, dst, [python(dst), '-c', code], timeout=240)
    want = ((b'0123456789abcdef' * 64 + b'\n') * (size // 1025 + 1))[:size]
    assert done.rc == 0 and len(done.out) == size and hashlib.sha256(done.out).digest() == hashlib.sha256(want).digest(), \
        'got %d bytes rc %d' % (len(done.out), done.rc)
    return '8 MiB output byte-exact'


TESTS = [('argv', t_argv), ('cmd', t_cmd), ('stdin', t_stdin), ('cwd', t_cwd), ('cwd-outside', t_cwd_outside), ('env', t_env_streams), ('exit', t_exit),
         ('pty', t_pty), ('big', t_big)]


def check(name, pair, action):
    key = 'X-17:%s:%s' % (name, pair)
    started = time.time()
    try:
        detail = action()
        results[key] = {'status': 'PASS', 'detail': detail or ''}
        print('PASS %s %ds %s' % (key, time.time() - started, detail or ''), flush=True)
    except Exception as error:  # noqa: BLE001
        results[key] = {'status': 'FAIL', 'detail': str(error)[:500]}
        print('FAIL %s %ds: %s' % (key, time.time() - started, str(error)[:500]), flush=True)


def t_offline(devices):
    """An offline target gives the documented message: the Linux container is frozen (docker pause) until the server shows it
    offline, the others exec into it, then it is thawed and waited for."""
    import subprocess
    target = devices['L']
    subprocess.run(['ssh', fleet.L_HOST, 'docker', 'pause', fleet.L_CT], check=True, capture_output=True, timeout=60)
    try:
        for key, src in devices.items():
            if key == 'L':
                continue
            for _ in range(60):
                listed = fleet.json_pb(src, 'machine', 'list', '--q', target.alias)
                if any(m['alias'] == target.alias and m['state'] != 'online' for m in listed['machines']):
                    break
                time.sleep(5)
            # the client's own inventory follows the server a few seconds later: the first attempt can still say 'reached'
            for attempt in range(1, 7):
                done = run(src, target, ['echo', 'hi'], timeout=90)
                if done.rc != 0 and 'offline' in (done.text + done.errtext).lower():
                    break
                time.sleep(10)
            check('offline', '%s>L' % key, lambda: _offline_message(done) + ' (attempt %d)' % attempt)
    finally:
        subprocess.run(['ssh', fleet.L_HOST, 'docker', 'unpause', fleet.L_CT], capture_output=True, timeout=60)
        for _ in range(60):
            listed = fleet.json_pb(next(iter(devices.values())), 'machine', 'list', '--q', target.alias)
            if any(m['alias'] == target.alias and m['state'] == 'online' for m in listed['machines']):
                break
            time.sleep(5)


def _offline_message(done):
    text = done.text + done.errtext
    assert done.rc != 0 and 'offline' in text.lower(), 'no offline message: %r' % text[-300:]
    return 'message: ' + text.strip().splitlines()[-1][:120]


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
    if 'L' in devices and (not only or 'offline' in only):
        t_offline(devices)
    stamp = time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())
    out = Path(__file__).parent / 'results' / ('xplat-exec-%s.json' % stamp)
    out.write_text(json.dumps({'check': 'xplat-exec', 'devices': keys,
                               'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
