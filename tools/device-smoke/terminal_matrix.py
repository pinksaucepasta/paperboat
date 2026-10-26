#!/usr/bin/env python3
"""Terminal quality matrix, native side: several real pb clients on one session under faults.

Runs on a signed-in client machine (the owner) against TARGET, another machine of the same
account. Every client is a real `pb connect` / `pb session attach` process under its own PTY.
Faults are injected locally: SIGKILL (a client leaves), SIGSTOP/SIGCONT (laptop sleep),
iptables (network loss, control plane unreachable), resize storms, 50 MB of output and a
large paste. Each row prints PASS/FAIL/SKIP with a short reason; output content is never
printed. Only sessions named smoke-matrix-* are created, and all are closed and deleted.

Usage: terminal_matrix.py TARGET [--only ROW,ROW] [--member-cmd CMD --team TEAM --member ID]
"""
import argparse
import hashlib
import os
from pathlib import Path
import re
import shlex
import signal
import subprocess
import sys
import time
import uuid

sys.path.insert(0, Path(__file__).resolve().parent.as_posix())
from session_cleanup import cleanup  # noqa: E402
from session_lifecycle import selected  # noqa: E402
from terminal_lifecycle import Terminal  # noqa: E402

PB = Path.home() / '.local/bin/pb'
RESULTS = []


def row(name, ok, detail=''):
    state = 'SKIP' if ok is None else ('PASS' if ok else 'FAIL')
    RESULTS.append((name, state))
    print(f'{state} {name}' + (f': {detail}' if detail else ''), flush=True)


class Client(Terminal):
    """One attached pb client. ready() waits for a shell that answers a marker."""

    def ready(self, timeout=90):
        # Keys typed before the shell enables bracketed paste are lost by its line editor.
        self.expect(b'\x1b[?2004h', timeout)
        self.expect(self.marker('READY', 'stty -echo; '), timeout)

    def run(self, command, timeout=60):
        """Run command in the shell and wait until it has finished."""
        token = uuid.uuid4().hex
        self.send(command + "; printf '\\nPB_DONE_%s\\n' '" + token + "'\r")
        return self.expect('PB_DONE_' + token, timeout)

    def run_discarding(self, command, timeout):
        """Like run, for output too large to keep: only a sliding tail is kept to find the marker."""
        import select
        token = ('PB_DONE_' + uuid.uuid4().hex).encode()
        self.send(command + "; printf '\\nPB_DONE_%s\\n' '" + token.decode()[8:] + "'\r")
        tail, deadline = b'', time.monotonic() + timeout
        while token not in tail:
            if time.monotonic() >= deadline:
                raise RuntimeError('terminal did not finish the command in time')
            if select.select([self.fd], [], [], 0.1)[0]:
                chunk = os.read(self.fd, 1 << 16)
                if not chunk:
                    raise RuntimeError('terminal closed')
                tail = (tail + chunk)[-4096:]
        self.pending.clear()

    def size(self):
        out = self.run('stty size')
        found = re.findall(rb'(\d+) (\d+)\r?\n', out)
        return tuple(map(int, found[-1])) if found else None


def connect(target, name, rows=24, cols=80):
    c = Client([PB, 'connect', target, '--name', name, '--status-bar', 'off'])
    c.resize(rows, cols)
    c.ready()
    return c


def attach(target, session, rows=24, cols=80):
    c = Client([PB, 'connect', target, '--session', session, '--status-bar', 'off'])
    c.resize(rows, cols)
    c.ready()
    return c


def fresh_name():
    return 'smoke-matrix-' + uuid.uuid4().hex[:10]


def lines(out, prefix):
    return re.findall(rb'(?m)^' + re.escape(prefix) + rb'(\d+)\r?$', out)


def exactly_once(out, prefix, first, last):
    """Numbered lines prefix<first..last> appear once each, in order (nothing lost or duplicated)."""
    got = [int(n) for n in lines(out, prefix)]
    want = list(range(first, last + 1))
    if got == want:
        return True, ''
    dup = len(got) - len(set(got))
    missing = len(set(want) - set(got))
    return False, f'{len(got)} lines, {dup} duplicated, {missing} missing'


def has_iptables():
    return subprocess.run(['sudo', '-n', 'iptables', '-S'], capture_output=True).returncode == 0


def iptables(*rule):
    subprocess.run(['sudo', '-n', 'iptables', *rule], check=True, capture_output=True)


# Rows. Each takes (args, sessions) and records its own result; sessions collects names to clean up.

def two_clients(a, sessions):
    """W1/W5: two windows of the same session: both see all output; the PTY size follows a rule."""
    name = fresh_name(); sessions.append(name)
    c1 = connect(a.target, name, 30, 100)
    try:
        _, s = selected(PB, a.target, name)
        c2 = attach(a.target, s['id'], 24, 80)
        try:
            token = uuid.uuid4().hex
            c2.send("printf '\\nPB_BOTH_%s\\n' '" + token + "'\r")
            c2.expect('PB_BOTH_' + token)
            seen = True
            try:
                c1.expect('PB_BOTH_' + token, 20)
            except RuntimeError:
                seen = False
            row('N1 two windows: output typed in one shows in the other', seen)
            s1, s2 = c1.size(), c2.size()
            row('N1 two windows: both report the same PTY size', s1 == s2, f'{s1} vs {s2}')
            c1.resize(40, 120)
            time.sleep(2)
            after = c2.size()
            row('N1 two windows: PTY size is one client\'s size (record policy)',
                after in [(40, 120), (24, 80), (30, 100)], f'after window 1 → 40x120: {after}')
        finally:
            c2.close()
    finally:
        c1.close()


def leave_rejoin(a, sessions):
    """W2/W8: a client leaves; output made meanwhile is replayed once on rejoin."""
    name = fresh_name(); sessions.append(name)
    c1 = connect(a.target, name)
    _, s = selected(PB, a.target, name)
    c2 = attach(a.target, s['id'])
    try:
        os.kill(c1.pid, signal.SIGKILL)
        c1.close()
        c2.run("seq -f 'LR%g' 1 2000", 60)
        c1 = attach(a.target, s['id'])
        c1.settle(2)
        # A fresh client joins at the live edge with a redraw and recent scrollback (as tmux does),
        # so the replay is a tail: it must be contiguous, end at the newest line and repeat nothing.
        got = [int(n) for n in lines(bytes(c1.output), b'LR')]
        ok = bool(got) and got[-1] == 2000 and got == list(range(got[0], 2001))
        row('N2 rejoin: replay is a contiguous tail ending at the newest line, nothing repeated', ok,
            f'{len(got)} of 2000 lines shown' if got else 'no lines')
        c1.run('true')
        row('N2 rejoin: the rejoined window types again', True)
    finally:
        c1.close()
        c2.close()


def sleep_wake(a, sessions):
    """W6: laptop sleep (SIGSTOP 90 s) while output continues; keys typed during sleep arrive once."""
    name = fresh_name(); sessions.append(name)
    c1 = connect(a.target, name)
    _, s = selected(PB, a.target, name)
    c2 = attach(a.target, s['id'])
    try:
        os.kill(c1.pid, signal.SIGSTOP)
        c2.run("for i in $(seq 1 90); do echo SW$i; sleep 1; done", 150)
        key = uuid.uuid4().hex
        c1.send("printf '\\nPB_KEY_%s\\n' '" + key + "'\r")  # sits in c1's PTY while it sleeps
        os.kill(c1.pid, signal.SIGCONT)
        t0 = time.monotonic()
        try:
            c1.expect('PB_KEY_' + key, 120)
            row('N3 wake: keys typed during sleep arrive', True, f'{time.monotonic() - t0:.1f} s after wake')
        except RuntimeError:
            row('N3 wake: keys typed during sleep arrive', False, 'not echoed within 120 s')
        c2.settle(2)
        count = bytes(c2.output).count(b'PB_KEY_' + key.encode())
        row('N3 wake: those keys ran exactly once', count == 1, f'{count} times')
        ok, why = exactly_once(bytes(c1.output), b'SW', 1, 90)
        row('N3 wake: output made during sleep is replayed exactly once', ok, why)
    finally:
        c1.close()
        c2.close()


def network_drop(a, sessions):
    """W6/W8: the client's network drops for 45 s while output continues on the host."""
    if not has_iptables():
        row('N4 network drop: session resumes, nothing lost or doubled', None, 'no iptables on this machine')
        return
    name = fresh_name(); sessions.append(name)
    c1 = connect(a.target, name)
    try:
        c1.send("for i in $(seq 1 60); do echo ND$i; sleep 1; done\r")
        time.sleep(5)
        iptables('-I', 'OUTPUT', '-o', 'lo', '-j', 'ACCEPT')
        iptables('-I', 'OUTPUT', '2', '-j', 'DROP')
        iptables('-I', 'INPUT', '-i', 'lo', '-j', 'ACCEPT')
        iptables('-I', 'INPUT', '2', '-j', 'DROP')
        try:
            time.sleep(45)
        finally:
            for chain, iface in [('OUTPUT', '-o'), ('INPUT', '-i')]:
                iptables('-D', chain, '-j', 'DROP')
                iptables('-D', chain, iface, 'lo', '-j', 'ACCEPT')
        t0 = time.monotonic()
        try:
            c1.expect('ND60', 120)
            row('N4 network drop: session resumes on its own', True, f'{time.monotonic() - t0:.1f} s after the network returned')
        except RuntimeError:
            row('N4 network drop: session resumes on its own', False, 'no output 120 s after the network returned')
            return
        c1.settle(2)
        ok, why = exactly_once(bytes(c1.output), b'ND', 1, 60)
        row('N4 network drop: output during the gap arrives exactly once', ok, why)
    finally:
        c1.close()


def control_plane_down(a, sessions):
    """W7: the server is unreachable (deploy, outage) mid-session; the open terminal keeps working."""
    if not has_iptables():
        row('N5 server unreachable: open terminal keeps working', None, 'no iptables on this machine')
        return
    import json
    import socket
    from urllib.parse import urlparse
    runtime = Path.home() / '.local/state/paperboat/runtime/machine-registration.json'
    host = urlparse(json.loads(runtime.read_text())['server_url']).hostname
    addrs = sorted({ai[4][0] for ai in socket.getaddrinfo(host, 443, socket.AF_INET)})
    name = fresh_name(); sessions.append(name)
    c1 = connect(a.target, name)
    try:
        for ip in addrs:
            iptables('-I', 'OUTPUT', '-d', ip, '-j', 'DROP')
        try:
            ok = True
            for i in range(4):
                time.sleep(30)
                try:
                    c1.run('echo alive', 30)
                except RuntimeError:
                    ok = False
                    break
            row('N5 server unreachable 2 min: open terminal keeps working', ok, f'{(i + 1) * 30} s')
        finally:
            for ip in addrs:
                iptables('-D', 'OUTPUT', '-d', ip, '-j', 'DROP')
    finally:
        c1.close()


def fast_output(a, sessions):
    """W9: 50 MB of output with a stalled second window; the first stays live, Ctrl-C works."""
    name = fresh_name(); sessions.append(name)
    c1 = connect(a.target, name)
    _, s = selected(PB, a.target, name)
    c2 = attach(a.target, s['id'])
    try:
        os.kill(c2.pid, signal.SIGSTOP)  # a stalled viewer must not stall others
        t0 = time.monotonic()
        try:
            c1.run_discarding('yes 0123456789012345678901234567890123456789012345678 | head -c 50000000', 300)
            row('N6 50 MB output finishes with a stalled second window', True, f'{time.monotonic() - t0:.0f} s')
        except RuntimeError:
            row('N6 50 MB output finishes with a stalled second window', False, 'not done in 300 s')
        c1.pending.clear()
        c1.send('yes\r')
        time.sleep(3)
        c1.send('\x03')
        t0 = time.monotonic()
        try:
            c1.run_discarding('true', 10)
            row('N6 Ctrl-C stops yes promptly', True, f'{time.monotonic() - t0:.1f} s')
        except RuntimeError:
            row('N6 Ctrl-C stops yes promptly', False, 'no prompt within 10 s')
        os.kill(c2.pid, signal.SIGCONT)
        try:
            c2.run('true', 60)
            row('N6 the stalled window recovers after it resumes', True)
        except RuntimeError:
            row('N6 the stalled window recovers after it resumes', False, 'not responsive 60 s after SIGCONT')
    finally:
        c1.close()
        c2.close()


def resize_storm(a, sessions):
    """W10: 200 resizes in about 2 s; the final size wins."""
    name = fresh_name(); sessions.append(name)
    c1 = connect(a.target, name)
    try:
        for i in range(200):
            c1.resize(20 + i % 30, 60 + (i * 7) % 100)
            time.sleep(0.01)
        c1.resize(37, 111)
        time.sleep(2)
        got = c1.size()
        row('N7 resize storm: the last size wins', got == (37, 111), str(got))
    finally:
        c1.close()


def large_paste(a, sessions):
    """W11: a 256 KiB paste reaches the program byte for byte; a bracketed paste at the shell
    prompt is inserted as one edit and runs only on Enter."""
    name = fresh_name(); sessions.append(name)
    c1 = connect(a.target, name)
    try:
        body = (uuid.uuid4().hex * 8 + '\n') * 1000  # 257,000 bytes of text lines
        want = hashlib.sha256(body.encode()).hexdigest()
        path = '/tmp/' + name + '.paste'
        c1.send('stty raw -echo; head -c ' + str(len(body)) + ' > ' + path + '; stty sane -echo\r')
        time.sleep(1)
        c1.send(body)
        out = c1.run('sha256sum ' + path + '; rm -f ' + path, 120)
        row('N8 256 KiB paste arrives byte for byte', want.encode() in out)
        c1.send('stty echo\r')
        c1.settle(1)
        one, two = uuid.uuid4().hex, uuid.uuid4().hex
        c1.send("\x1b[200~printf '%s_%s\\n' PB_P1 " + one + "\nprintf '%s_%s\\n' PB_P2 " + two + "\x1b[201~")
        c1.settle(2)
        early = b'PB_P1_' + one.encode() in bytes(c1.output)
        c1.send('\r')
        try:
            c1.expect('PB_P2_' + two, 30)
            ran = bytes(c1.output).count(b'PB_P1_' + one.encode()) == 1
        except RuntimeError:
            ran = False
        row('N8b bracketed paste at the prompt waits for Enter, then runs each line once', not early and ran,
            'ran before Enter' if early else ('' if ran else 'did not run once after Enter'))
        c1.send('stty -echo\r')
    finally:
        c1.close()


def many_sessions(a, sessions):
    """W13: 20 sessions open at once from one machine all work."""
    clients = []
    try:
        failed = 0
        for _ in range(20):
            name = fresh_name(); sessions.append(name)
            try:
                clients.append(connect(a.target, name))
            except RuntimeError:
                failed += 1
        working = 0
        for c in clients:
            try:
                c.run('true', 30)
                working += 1
            except RuntimeError:
                pass
        row('N9 20 sessions open at once all work', failed == 0 and working == 20, f'{working}/20 working')
    finally:
        for c in clients:
            c.close()


def member(a, args, timeout=60):
    """Run pb as the team member on the member machine (MEMBER_CMD prefixes the remote command)."""
    remote = ' '.join(shlex.quote(x) for x in ['timeout', str(timeout), '/home/pbt/.local/bin/pb', *args])
    return subprocess.run(shlex.split(a.member_cmd) + [remote], capture_output=True, text=True, timeout=timeout + 30)


def shared(a, sessions):
    """W1/W3: a second user joins interactive, then as viewer; the owner unshares."""
    if not a.member_cmd:
        row('N10 shared session: second user types / viewer cannot', None, 'no --member-cmd')
        return
    name = fresh_name(); sessions.append(name)
    c1 = connect(a.target, name)
    joined = None
    try:
        _, s = selected(PB, a.target, name)
        share = ['--workspace', 'personal', 'session', 'share', s['id'], '--team', a.team, '--member', a.member]
        subprocess.run([PB, *share, '--role', 'interactive', '--json'], check=True, capture_output=True, timeout=60)
        joined = Client(shlex.split(a.member_cmd) + ['/home/pbt/.local/bin/pb', 'session', 'join', s['id']])
        token = uuid.uuid4().hex
        joined.send("printf '\\nPB_MEMBER_%s\\n' '" + token + "'\r")
        try:
            c1.expect('PB_MEMBER_' + token, 60)
            row('N10 interactive member: their typing reaches the owner', True)
        except RuntimeError:
            row('N10 interactive member: their typing reaches the owner', False, 'not seen by the owner in 60 s')
        joined.close()
        subprocess.run([PB, *share, '--role', 'viewer', '--json'], check=True, capture_output=True, timeout=60)
        joined = Client(shlex.split(a.member_cmd) + ['/home/pbt/.local/bin/pb', 'session', 'join', s['id']])
        time.sleep(5)
        token = uuid.uuid4().hex
        joined.send("printf '\\nPB_VIEWER_%s\\n' '" + token + "'\r")
        c1.run('sleep 5', 30)
        row('N10 viewer: their typing does not reach the shell',
            b'PB_VIEWER_' + token.encode() not in bytes(c1.output))
        live = uuid.uuid4().hex
        c1.send("printf '\\nPB_LIVE_%s\\n' '" + live + "'\r")
        try:
            joined.expect('PB_LIVE_' + live, 30)
            row('N10 viewer: sees the owner\'s live output', True)
        except RuntimeError:
            row('N10 viewer: sees the owner\'s live output', False)
        subprocess.run([PB, '--workspace', 'personal', 'session', 'unshare', s['id'], '--json'],
                       check=True, capture_output=True, timeout=60)
        try:
            joined.wait_exit(60)
            row('N10 unshare ends the member\'s view', True, f'exit {joined.status}')
        except RuntimeError:
            row('N10 unshare ends the member\'s view', False, 'still attached 60 s after unshare')
    finally:
        if joined is not None:
            joined.close()
        c1.close()


ROWS = {'two': two_clients, 'rejoin': leave_rejoin, 'sleep': sleep_wake, 'net': network_drop,
        'server': control_plane_down, 'fast': fast_output, 'resize': resize_storm,
        'paste': large_paste, 'many': many_sessions, 'shared': shared}


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('target')
    p.add_argument('--only', default=','.join(ROWS))
    p.add_argument('--member-cmd', default='')
    p.add_argument('--team', default='')
    p.add_argument('--member', default='', help='member account ID')
    a = p.parse_args()
    sessions = []
    try:
        for key in a.only.split(','):
            try:
                ROWS[key](a, sessions)
            except Exception as error:  # one row's fixture failure must not hide the others
                row(f'{key} fixture', False, type(error).__name__ + ': ' + str(error)[:200])
    finally:
        for name in sessions:
            try:
                cleanup(str(PB), a.target, name)
            except Exception as error:
                print(f'cleanup {name}: {error}', file=sys.stderr)
    failed = [n for n, s in RESULTS if s == 'FAIL']
    print('RESULT', 'PASS' if not failed else 'FAIL ' + str(len(failed)))
    return 1 if failed else 0


if __name__ == '__main__':
    sys.exit(main())
