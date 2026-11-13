#!/usr/bin/env python3
"""X-16 and the `pb connect` leg of X-11: a terminal from each OS to each other OS, both directions.

  xplat_term.py [--devices L,M,W] [--only shell,utf8,resize,ctrlc,ctrld,exit,replay,env]

The controller drives a real pseudo-terminal with pexpect: an `ssh -tt` hop (or `docker exec -it`) into the client device
runs `pb connect <target>`, so the client's own terminal handling is what is tested, and the target's default shell answers.
Results: tools/device-smoke/results/xplat-term-<stamp>.json (`X-16:utf8:L>W`, `X-11:connect:L>W`).
"""
import hashlib
import json
import os
import re
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
        user, uid = os.environ.get('XPLAT_L_USER', 'tester'), os.environ.get('XPLAT_L_UID', '1001')
        return ['ssh', '-tt', fleet.L_HOST, *os.environ.get('XPLAT_L_DOCKER', 'docker').split(), 'exec', '-it', '-u', user, '-e', 'HOME=/home/' + user,
                '-e', 'XDG_RUNTIME_DIR=/run/user/' + uid, '-e', 'TERM=xterm-256color', fleet.L_CT, *tail]
    if src.os == 'windows':
        quoted = ' '.join("'%s'" % part.replace("'", "''") for part in tail)
        # Windows OpenSSH returns 0 for every `ssh -tt` login whatever the command exits with: the exit code of pb connect
        # is printed in band (RC=<n>) and read from the screen
        return ['ssh', '-tt', src.device.target, 'powershell -NoProfile -Command "& ' + quoted.replace('"', '\\"') + "; Write-Output ('RC=' + $LASTEXITCODE)\""]
    host = os.environ.get('XPLAT_MAC_SSH')
    if host:
        return ['ssh', '-tt', host, 'TERM=xterm-256color ' + ' '.join("'%s'" % part for part in tail)]
    # the Mac VM: its login keychain is unlocked per command, and its host key is not in known_hosts
    return ['ssh', '-tt', '-o', 'StrictHostKeyChecking=no', '-o', 'UserKnownHostsFile=/dev/null', '-o', 'LogLevel=ERROR', '-o', 'BatchMode=yes',
            'admin@100.88.169.42', fleet.mac_unlock() + 'TERM=xterm-256color ' + ' '.join("'%s'" % part for part in tail)]


class Terminal:
    """A pb connect terminal. `kind` is the target shell: unix, cmd or powershell (a Windows target's default shell is decided
    by what answers). Markers are built so the echo of the typed command can never match the expected output."""

    def __init__(self, src, dst, extra=()):
        self.src, self.dst = src, dst
        argv = client_argv(src, [src.pb_path, 'connect', dst.alias, *extra])
        self.child = pexpect.spawn(argv[0], argv[1:], encoding='utf-8', codec_errors='replace', timeout=90, dimensions=(30, 100))
        self.kind = 'unix' if dst.os != 'windows' else None
        self.unix = self.kind == 'unix'
        self.computer = dst.run(['hostname']).text.strip() if dst.os == 'windows' else ''

    def say(self, text):
        self.child.send(text + '\r')

    def expect(self, pattern, timeout=60):
        try:
            self.child.expect(pattern, timeout=timeout)
        except pexpect.TIMEOUT:
            raise AssertionError('timed out waiting for %r; screen tail: %r' % (pattern, (self.child.before or '')[-300:]))

    def echo(self, token):
        """(command, regex of its output). The output is computed by the shell, so the echo of the typed command cannot match.
        COMPUTERNAME is not in a pb terminal's Windows environment: cmd computes with `set /a`, PowerShell with an expression."""
        if self.kind == 'unix':
            return 'echo M_%s_$((6*7))' % token, 'M_%s_42' % token
        if self.kind == 'powershell':
            return 'echo ("M_%s_" + (6*7))' % token, 'M_%s_42' % token
        return 'echo M_%s_& set /a 6*7' % token, r'M_%s_\s+(?:\x1b\][^\x07]*\x07\s*)*42' % token  # cmd's own window-title sequences may sit between the two lines

    def ready(self):
        kinds = [self.kind] if self.kind else ['cmd', 'powershell']
        for attempt in range(8):
            for kind in kinds:
                self.kind = kind
                self.unix = kind == 'unix'
                command, want = self.echo(uuid.uuid4().hex[:6])
                self.say(command)
                try:
                    self.child.expect(want, timeout=10)
                    return
                except pexpect.TIMEOUT:
                    continue
                except pexpect.EOF:  # pb connect itself ended: its own message says why
                    raise AssertionError('pb connect ended before the target shell answered: %r' % (self.child.before or '')[-700:])
        raise AssertionError('the target shell never answered; screen tail: %r' % (self.child.before or '')[-300:])

    def close(self):
        try:
            self.child.close(force=True)
        except Exception:  # noqa: BLE001
            pass
        # `docker exec` keeps the client alive when the ssh hop dies: reap this device's leftover `pb connect` clients, or
        # hundreds of them end up holding terminal sessions and the machine's own pb can no longer open a transport
        if self.src.os != 'windows':
            self.src.sh("ps -eo pid,args | awk '$2 ~ /\\/pb$/ && $3==\"connect\" {print $1}' | xargs -r kill", timeout=60)


def t_shell(src, dst):
    term = Terminal(src, dst)
    try:
        term.ready()
        return 'the target shell started and answered (%s)' % term.kind
    finally:
        term.close()


def t_utf8(src, dst):
    term = Terminal(src, dst)
    try:
        term.ready()
        wide = '\u03bb\u8239\U0001F600'
        command = {'unix': "a='%s'; printf 'U:%%s:E\\n' \"$a\"" % wide, 'cmd': 'set a=%s& call echo U:%%a%%:E' % wide,
                   'powershell': "$a='%s'; echo \"U:$a:E\"" % wide}[term.kind]
        term.say(command)
        term.expect('U:%s:E' % wide)
        return 'UTF-8 and wide characters typed and echoed unchanged (%s)' % term.kind
    finally:
        term.close()


def t_resize(src, dst):
    term = Terminal(src, dst, ['--status-bar', 'off'])  # the status bar takes a row of the window: compare without it
    try:
        term.ready()
        term.child.setwinsize(45, 132)
        time.sleep(3)
        term.say({'unix': 'stty size', 'cmd': 'mode con', 'powershell': '"{0}x{1}" -f $Host.UI.RawUI.WindowSize.Height,$Host.UI.RawUI.WindowSize.Width'}[term.kind])
        term.expect({'unix': r'45 132', 'cmd': r'Lines:\s+45', 'powershell': r'45x132'}[term.kind])
        return 'a window resize reached the remote PTY (45x132, %s)' % term.kind
    finally:
        term.close()


SLEEP = {'unix': 'sleep 60', 'cmd': 'ping -n 60 127.0.0.1 >nul', 'powershell': 'Start-Sleep 60'}


def t_ctrlc(src, dst):
    term = Terminal(src, dst)
    try:
        term.ready()
        term.say(SLEEP[term.kind])
        time.sleep(3)
        term.child.send('\x03')
        time.sleep(2)
        term.ready()
        return 'Ctrl-C stopped a long command and the shell answered again (%s)' % term.kind
    finally:
        term.close()


def t_ctrld(src, dst):
    """Ctrl-D reaches the target shell exactly as typed locally: a unix shell at an empty prompt exits (pb connect ends with 0),
    cmd.exe and PowerShell do not treat it as end of input, so the session stays usable."""
    term = Terminal(src, dst)
    try:
        term.ready()
        term.child.send('\x04')
        if term.kind == 'unix':
            if term.src.os == 'windows':
                term.expect(r'RC=0', timeout=60)
            else:
                term.child.expect(pexpect.EOF, timeout=60)
                term.child.close()
                assert term.child.exitstatus == 0, 'Ctrl-D ended the unix shell with status %r' % (term.child.exitstatus,)
            return 'Ctrl-D at an empty prompt ended the unix shell and pb connect exited 0'
        time.sleep(2)
        term.ready()
        return 'Ctrl-D did not end the %s session, which stays usable like a local one' % term.kind
    finally:
        term.close()


def t_exit(src, dst):
    term = Terminal(src, dst)
    try:
        term.ready()
        term.say('exit 3')
        if term.src.os == 'windows':
            term.expect(r'RC=3', timeout=60)
            status = 3
        else:
            term.child.expect(pexpect.EOF, timeout=60)
            term.child.close()
            status = term.child.exitstatus
        assert status == 3, 'the exit code 3 of the remote shell was reported as %r' % (status,)
        return 'the remote exit code (3) is the exit status of pb connect (%s)' % term.kind
    finally:
        term.close()


def t_replay(src, dst):
    name = 'xplat-' + uuid.uuid4().hex[:6]
    token = uuid.uuid4().hex[:8]
    first = Terminal(src, dst, ['new', '--name', name])
    try:
        first.ready()
        command, want = first.echo('REPLAY_' + token)
        first.say(command)
        first.expect(want)
        first.say(SLEEP[first.kind].replace('60', '45'))
        time.sleep(2)
    finally:
        first.close()  # the client goes away mid-command: the session must live on
    time.sleep(5)
    second = Terminal(src, dst, ['--session', name])
    second.kind = first.kind
    try:
        second.expect(want, timeout=60)
        second.say('exit')
        return 'after the client vanished, a reattach replayed the earlier output of a session that kept running (%s)' % first.kind
    finally:
        second.close()


def t_env(src, dst):
    """X-11: the ENV set once is in a new `pb connect` terminal, byte-equal (sha256 per value)."""
    assert VALUES, 'xplat_env is not importable'
    term = Terminal(src, dst)
    try:
        term.ready()
        code = "import os,hashlib;[print('H:'+k+':'+hashlib.sha256(os.environ[k].encode()).hexdigest()) for k in sorted(os.environ) if k.startswith('XP_')]"
        term.say(("python3 -c " if term.kind == 'unix' else "python -c ") + '"%s"' % code)
        for name, value in VALUES.items():
            term.expect('H:%s:%s' % (name, hashlib.sha256(value.encode()).hexdigest()), timeout=60)
        return 'all %d values equal (sha256) in a new pb connect terminal on %s (%s)' % (len(VALUES), dst.name, term.kind)
    finally:
        term.close()


def t_join(src, dst, third):
    """A shared terminal: a session started from `src` is joined live from `third` (the other OS) and shows the same output."""
    name = 'xplat-' + uuid.uuid4().hex[:6]
    token = uuid.uuid4().hex[:6]
    first = Terminal(src, dst, ['new', '--name', name])
    second = None
    try:
        first.ready()
        command, want = first.echo('JOINA_' + token)
        first.say(command)
        first.expect(want)
        second = Terminal(third, dst, ['--session', name])
        second.kind = first.kind
        second.expect(want, timeout=60)  # what was already printed is replayed to the joiner
        command, want = first.echo('JOINB_' + token)
        first.say(command)
        second.expect(want, timeout=60)  # and what is typed later reaches the joiner live
        return 'a terminal started from %s was joined from %s: earlier and live output identical (%s)' % (src.name, third.name, first.kind)
    finally:
        first.close()
        if second:
            second.close()


TESTS = [('shell', t_shell, 'X-16'), ('utf8', t_utf8, 'X-16'), ('resize', t_resize, 'X-16'), ('ctrlc', t_ctrlc, 'X-16'), ('ctrld', t_ctrld, 'X-16'),
         ('exit', t_exit, 'X-16'), ('replay', t_replay, 'X-16'), ('connect', t_env, 'X-11'), ('join', t_join, 'X-16')]


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
    if VALUES and (not only or 'connect' in only):
        xplat_env.provision(devices)  # the connect check reads values another row's vault test may have removed
    for a_key, b_key in permutations(devices, 2):
        for name, test, row in TESTS:
            if only and name not in only:
                continue
            if name == 'join':
                third = [k for k in devices if k not in (a_key, b_key)]
                if third:
                    check(row, name, '%s>%s' % (a_key, b_key), lambda: test(devices[a_key], devices[b_key], devices[third[0]]))
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
