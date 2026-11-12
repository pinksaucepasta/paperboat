#!/usr/bin/env python3
"""X-16 and the `pb connect` leg of X-11: a terminal from each OS to each other OS, both directions.

  xplat_term.py [--devices L,M,W] [--only shell,utf8,resize,ctrlc,exit,replay,env]

The controller drives a real pseudo-terminal with pexpect: an `ssh -tt` hop (or `docker exec -it`) into the client device
runs `pb connect <target>`, so the client's own terminal handling is what is tested, and the target's default shell answers.
Results: tools/device-smoke/results/xplat-term-<stamp>.json (`X-16:utf8:L>W`, `X-11:connect:L>W`).
"""
import hashlib
import json
import os
import sys
import time
import uuid
from itertools import permutations
from pathlib import Path

import pexpect

sys.path.insert(0, str(Path(__file__).parent))
import xplat_fleet as fleet  # noqa: E402

results = {}
VALUES = {}
try:
    import xplat_env
    VALUES = xplat_env.VALUES
except ImportError:  # the env leg is optional
    pass


def client_argv(src, tail):
    """The local command that opens a terminal on `src` and runs `tail` (a list) there."""
    if src.os == 'linux':
        return ['ssh', '-tt', fleet.L_HOST, 'docker', 'exec', '-it', '-u', 'tester', '-e', 'HOME=/home/tester', '-e', 'XDG_RUNTIME_DIR=/run/user/1001',
                '-e', 'TERM=xterm-256color', fleet.L_CT, *tail]
    if src.os == 'windows':
        quoted = ' '.join("'%s'" % part.replace("'", "''") for part in tail)
        return ['ssh', '-tt', src.device.target, 'powershell -NoProfile -Command "& ' + quoted.replace('"', '\\"') + '; exit `$LASTEXITCODE"']
    host = os.environ.get('XPLAT_MAC_SSH')
    return ['ssh', '-tt', host or 'admin@100.88.169.42', 'TERM=xterm-256color ' + ' '.join("'%s'" % part for part in tail)]


class Terminal:
    def __init__(self, src, dst, extra=()):
        self.src, self.dst = src, dst
        self.child = pexpect.spawn(client_argv(src, [src.pb_path, 'connect', dst.alias, *extra])[0], client_argv(src, [src.pb_path, 'connect', dst.alias, *extra])[1:],
                                   encoding='utf-8', codec_errors='replace', timeout=90, dimensions=(30, 100))
        self.unix = dst.os != 'windows'

    def say(self, text):
        self.child.send(text + '\r')

    def expect(self, pattern, timeout=60):
        try:
            self.child.expect(pattern, timeout=timeout)
        except pexpect.TIMEOUT:
            raise AssertionError('timed out waiting for %r; screen tail: %r' % (pattern, (self.child.before or '')[-300:]))

    def marker(self):
        """A command whose output cannot be confused with its echo, for the target's shell."""
        token = uuid.uuid4().hex[:6]
        return token, ('echo M_%s_$((6*7))' % token) if self.unix else ('echo ("M_%s_" + (6*7))' % token), 'M_%s_42' % token

    def ready(self):
        token, command, want = self.marker()
        for _ in range(8):
            self.say(command)
            try:
                self.child.expect(want, timeout=12)
                return
            except pexpect.TIMEOUT:
                continue
        raise AssertionError('the target shell never answered; screen tail: %r' % (self.child.before or '')[-300:])

    def close(self):
        try:
            self.child.close(force=True)
        except Exception:  # noqa: BLE001
            pass


def t_shell(src, dst):
    term = Terminal(src, dst)
    try:
        term.ready()
        term.say('echo $0' if term.unix else '$PSVersionTable.PSVersion.Major')
        term.expect(r'(bash|zsh|sh)' if term.unix else r'\b[57]\b')
        return 'the target shell started and answered (%s)' % ('unix shell' if term.unix else 'PowerShell')
    finally:
        term.close()


def t_utf8(src, dst):
    term = Terminal(src, dst)
    try:
        term.ready()
        text = 'U:\u03bb\u8239\U0001F600:E'
        term.say("printf 'U:%s:E\\n' '\u03bb\u8239\U0001F600'" if term.unix else "Write-Output 'U:\u03bb\u8239\U0001F600:E'")
        term.expect(text.replace('U:', 'U:', 1).replace('\U0001F600', '\U0001F600'))
        return 'UTF-8 and wide characters came back unchanged'
    finally:
        term.close()


def t_resize(src, dst):
    term = Terminal(src, dst, ['--status-bar', 'off'])  # the status bar takes a row of the window: compare without it
    try:
        term.ready()
        term.child.setwinsize(45, 132)
        time.sleep(3)
        term.say('stty size' if term.unix else '"{0}x{1}" -f $Host.UI.RawUI.WindowSize.Height,$Host.UI.RawUI.WindowSize.Width')
        term.expect(r'45 132' if term.unix else r'45x132')
        return 'a window resize reached the remote PTY (45x132)'
    finally:
        term.close()


def t_ctrlc(src, dst):
    term = Terminal(src, dst)
    try:
        term.ready()
        term.say('sleep 60' if term.unix else 'Start-Sleep 60')
        time.sleep(3)
        term.child.send('\x03')
        time.sleep(2)
        term.ready()
        return 'Ctrl-C stopped a long command and the shell answered again'
    finally:
        term.close()


def t_exit(src, dst):
    term = Terminal(src, dst)
    try:
        term.ready()
        term.say('exit 3')
        term.child.expect(pexpect.EOF, timeout=60)
        term.child.close()
        status = term.child.exitstatus
        assert status == 3, 'the exit code 3 of the remote shell was reported as %r' % (status,)
        return 'the remote exit code (3) is the exit status of pb connect'
    finally:
        term.close()


def t_replay(src, dst):
    name = 'xplat-' + uuid.uuid4().hex[:6]
    token = uuid.uuid4().hex[:8]
    first = Terminal(src, dst, ['new', '--name', name])
    try:
        first.ready()
        first.say(('echo REPLAY_%s_$((2*21))' % token) if first.unix else ('echo ("REPLAY_%s_" + (2*21))' % token))
        first.expect('REPLAY_%s_42' % token)
        first.say('sleep 45' if first.unix else 'Start-Sleep 45')
        time.sleep(2)
    finally:
        first.close()  # the client goes away mid-command: the session must live on
    time.sleep(5)
    second = Terminal(src, dst, ['--session', name])
    try:
        second.expect('REPLAY_%s_42' % token, timeout=60)
        second.say('exit')
        return 'after the client vanished, a reattach replayed the earlier output of a session that kept running'
    finally:
        second.close()


def t_env(src, dst):
    """X-11: the ENV set once is in a new `pb connect` terminal, byte-equal (sha256 per value)."""
    assert VALUES, 'xplat_env is not importable'
    term = Terminal(src, dst)
    try:
        term.ready()
        if term.unix:
            term.say("python3 -c 'import os,hashlib;[print(\"H:%s:%s\" % (k, hashlib.sha256(os.environ[k].encode()).hexdigest())) for k in sorted(os.environ) if k.startswith(\"XP_\")]'")
        else:
            term.say("python -c \"import os,hashlib;[print('H:%s:%s' % (k, hashlib.sha256(os.environ[k].encode()).hexdigest())) for k in sorted(os.environ) if k.startswith('XP_')]\"")
        for name, value in VALUES.items():
            term.expect('H:%s:%s' % (name, hashlib.sha256(value.encode()).hexdigest()), timeout=60)
        return 'all %d values equal (sha256) in a new pb connect terminal on %s' % (len(VALUES), dst.name)
    finally:
        term.close()


TESTS = [('shell', t_shell, 'X-16'), ('utf8', t_utf8, 'X-16'), ('resize', t_resize, 'X-16'), ('ctrlc', t_ctrlc, 'X-16'),
         ('exit', t_exit, 'X-16'), ('replay', t_replay, 'X-16'), ('connect', t_env, 'X-11')]


def check(row, name, pair, action):
    key = '%s:%s:%s' % (row, name, pair)
    started = time.time()
    try:
        detail = action()
        results[key] = {'status': 'PASS', 'detail': detail}
        print('PASS %s %ds %s' % (key, time.time() - started, detail[:300]), flush=True)
    except Exception as error:  # noqa: BLE001
        results[key] = {'status': 'FAIL', 'detail': str(error)[:700]}
        print('FAIL %s %ds: %s' % (key, time.time() - started, str(error)[:700]), flush=True)


def main():
    argv = sys.argv[1:]
    keys = argv[argv.index('--devices') + 1].split(',') if '--devices' in argv else ['L', 'M', 'W']
    only = set(argv[argv.index('--only') + 1].split(',')) if '--only' in argv else set()
    devices = {k: d for k, d in fleet.fleet().items() if k in keys}
    for a_key, b_key in permutations(devices, 2):
        for name, test, row in TESTS:
            if only and name not in only:
                continue
            check(row, name, '%s>%s' % (a_key, b_key), lambda: test(devices[a_key], devices[b_key]))
    stamp = time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())
    out = Path(__file__).parent / 'results' / ('xplat-term-%s.json' % stamp)
    out.write_text(json.dumps({'check': 'xplat-term', 'devices': keys, 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
