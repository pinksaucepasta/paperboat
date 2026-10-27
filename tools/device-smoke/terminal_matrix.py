#!/usr/bin/env python3
"""Terminal quality matrix, native side: several real pb clients on one session under faults.

Runs on a signed-in client machine (the owner) against TARGET, another machine of the same
account. Every client is a real `pb connect` / `pb session attach` process under its own PTY.
Faults are injected locally: SIGKILL (a client leaves), SIGSTOP/SIGCONT (laptop sleep),
iptables (network loss, control plane unreachable), resize storms, 50 MB of output and a
large paste. Each row prints PASS/FAIL/SKIP with a short reason; output content is never
printed. Only sessions named smoke-matrix-* are created, and all are closed and deleted.

Usage: terminal_matrix.py TARGET [--only ROW,ROW] [--member-cmd CMD --team TEAM --member ID]
       [--restart-target-cmd CMD --restart-client-cmd CMD]   (N14: commands that restart the target host runtime / this daemon)
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

    def quiet_after(self, quiet, timeout):
        """Seconds until output has been quiet for `quiet` s (output discarded), or None."""
        import select
        t0 = time.monotonic()
        last = t0
        while time.monotonic() - t0 < timeout:
            if select.select([self.fd], [], [], 0.1)[0]:
                if not os.read(self.fd, 1 << 16):
                    return None
                last = time.monotonic()
            elif time.monotonic() - last >= quiet:
                return last - t0
        return None

    def size(self):
        out = self.run('stty size')
        found = re.findall(rb'(\d+) (\d+)\r?\n', out)
        return tuple(map(int, found[-1])) if found else None


def connect(target, name, rows=24, cols=80):
    c = Client([PB, 'connect', target, '--name', name, '--status-bar', 'off'])
    c.resize(rows, cols)
    c.ready()
    return c


def attach(target, session, rows=24, cols=80, typing=True):
    c = Client([PB, 'connect', target, '--session', session, '--status-bar', 'off'])
    c.resize(rows, cols)
    if typing:
        c.ready()
    else:  # ready() types its marker, which is activity; a silent reconnect only waits for the replayed prompt
        c.expect(b'\x1b[?2004h', 90)
    return c


def fresh_name():
    return 'smoke-matrix-' + uuid.uuid4().hex[:10]


def lines(out, prefix):
    # Control sequences are not text: a resize report (CSI 8 t) can start the line that follows it.
    out = re.sub(rb'\x1b\[[0-9;?]*[A-Za-z]', b'', out)
    return re.findall(rb'(?m)^' + re.escape(prefix) + rb'(\d+)\r?$', out)


def exactly_once(out, prefix, first, last):
    """Numbered lines prefix<first..last> appear once each, in order (nothing lost or duplicated)."""
    got = [int(n) for n in lines(out, prefix)]
    want = list(range(first, last + 1))
    if got == want:
        return True, ''
    dup = len(got) - len(set(got))
    missing = len(set(want) - set(got))
    gone = sorted(set(want) - set(got))[:5]
    return False, f'{len(got)} lines, {dup} duplicated, {missing} missing' + (f' (first missing: {gone})' if gone else '')


def has_iptables():
    return subprocess.run(['sudo', '-n', 'iptables', '-S'], capture_output=True).returncode == 0


def iptables(*rule):
    subprocess.run(['sudo', '-n', 'iptables', *rule], check=True, capture_output=True)


# Rows. Each takes (args, sessions) and records its own result; sessions collects names to clean up.

def pty_size(target, tty):
    """The session's PTY size read from outside it (pb exec), so measuring never counts as activity."""
    out = subprocess.run([str(PB), 'exec', target, '--', 'stty', '-F', tty, 'size'], capture_output=True, text=True, timeout=60).stdout
    found = re.findall(r'(\d+) (\d+)', out)
    return tuple(map(int, found[-1])) if found else None


def settled_size(target, tty, want, seconds=10):
    deadline, size = time.monotonic() + seconds, None
    while time.monotonic() < deadline:
        size = pty_size(target, tty)
        if size == want:
            break
        time.sleep(0.5)
    return size


def session_tty(c):
    out = c.run('tty')
    found = re.findall(rb'(/dev/pts/\d+)', out)
    return found[-1].decode() if found else None


def two_clients(a, sessions):
    """W1/W5, F3-17: two windows of one session both see all output, and the PTY has the size of the window that
    was last active (typing; tmux window-size latest): attaching does not take it and a resize from the other
    window does not apply."""
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
            tty = session_tty(c2)  # typing in window 2: it is the latest active
            size = settled_size(a.target, tty, (24, 80))
            row('N1 two windows: the size is the window that typed last (window 2)', size == (24, 80), f'{size}')
            c1.run('true')
            size = settled_size(a.target, tty, (30, 100))
            row('N1 two windows: typing moves the size to that window (window 1)', size == (30, 100), f'{size}')
            c2.resize(20, 70)
            time.sleep(2)
            size = pty_size(a.target, tty)
            row('N1 two windows: the other window resizing does not change it', size == (30, 100), f'{size}')
            c1.resize(40, 120)
            size = settled_size(a.target, tty, (40, 120))
            row('N1 two windows: the active window resizing does', size == (40, 120), f'{size}')
        finally:
            c2.close()
    finally:
        c1.close()


def reconnect_keeps_control(a, sessions):
    """N15, F3-17: a window that reconnects does not take the size from the window in use."""
    name = fresh_name(); sessions.append(name)
    c1 = connect(a.target, name, 30, 100)
    try:
        _, s = selected(PB, a.target, name)
        c2 = attach(a.target, s['id'], 24, 80)
        try:
            tty = session_tty(c1)  # window 1 is in use
            before = settled_size(a.target, tty, (30, 100))
            c2.close()
            c2 = attach(a.target, s['id'], 24, 80, typing=False)  # window 2 reconnects without typing
            time.sleep(3)
            after = pty_size(a.target, tty)
            row('N15 a reconnecting window never takes control', before == (30, 100) and after == (30, 100), f'{before} -> {after}')
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


def black_hole(a, sessions):
    """N13 (F3-15): the network silently drops every packet for 60 s (no reset, no link-down). Output continues on
    the host and a command is typed meanwhile: it resumes on its own, nothing lost or doubled, the keys run once."""
    if not has_iptables():
        row('N13 black hole: session resumes, nothing lost or doubled', None, 'no iptables on this machine')
        return
    name = fresh_name(); sessions.append(name)
    c1 = connect(a.target, name)
    try:
        c1.send("for i in $(seq 1 90); do echo BH$i; sleep 1; done\r")
        time.sleep(5)
        iptables('-I', 'OUTPUT', '-o', 'lo', '-j', 'ACCEPT')
        iptables('-I', 'OUTPUT', '2', '-j', 'DROP')
        iptables('-I', 'INPUT', '-i', 'lo', '-j', 'ACCEPT')
        iptables('-I', 'INPUT', '2', '-j', 'DROP')
        key = uuid.uuid4().hex
        dropped, seen, detected, typed = time.monotonic(), len(c1.output), None, False
        try:
            while time.monotonic() - dropped < 60:
                c1.settle(0.5)  # reads the pty: output is only collected while reading
                if detected is None and b'reconnect' in bytes(c1.output)[seen:].lower():
                    detected = time.monotonic() - dropped
                if not typed and time.monotonic() - dropped >= 20:
                    c1.send("printf '\\nPB_KEY_%s\\n' '" + key + "'\r")  # typed into the black hole
                    typed = True
        finally:
            for chain, iface in [('OUTPUT', '-o'), ('INPUT', '-i')]:
                iptables('-D', chain, '-j', 'DROP')
                iptables('-D', chain, iface, 'lo', '-j', 'ACCEPT')
        row('N13 black hole 60 s: the client shows it is reconnecting within 30 s', detected is not None and detected < 30,
            f'{detected:.1f} s' if detected is not None else 'never shown while the network was down')
        t0 = time.monotonic()
        try:
            c1.expect('BH90', 150)
            row('N13 black hole 60 s: session resumes on its own', True, f'{time.monotonic() - t0:.1f} s after the network returned')
        except RuntimeError:
            row('N13 black hole 60 s: session resumes on its own', False, 'no output 150 s after the network returned')
            return
        c1.settle(3)
        out = bytes(c1.output)
        ok, why = exactly_once(out, b'BH', 1, 90)
        row('N13 black hole 60 s: output during the gap arrives exactly once', ok, why)
        count = out.count(b'PB_KEY_' + key.encode())
        row('N13 black hole 60 s: keys typed meanwhile ran exactly once', count == 1, f'{count} times')
    finally:
        c1.close()


def run_shell(command, timeout=120):
    result = subprocess.run(command, shell=True, capture_output=True, timeout=timeout)
    if result.returncode != 0:
        raise RuntimeError(f'restart command failed ({result.returncode})')


def restarts(a, sessions):
    """N14 (F3-18): the host runtime, then this machine's daemon, restart while a terminal runs: the shell and its
    scrollback survive, the terminal recovers on its own, nothing typed or printed is lost or repeated."""
    if not a.restart_target_cmd or not a.restart_client_cmd:
        row('N14 runtime and daemon restarts', None, 'needs --restart-target-cmd and --restart-client-cmd')
        return
    deadline = time.monotonic() + 180  # a target still recovering from an earlier restart is not this row's fault
    while subprocess.run([PB, 'exec', a.target, '--', 'true'], capture_output=True, timeout=60).returncode != 0:
        if time.monotonic() > deadline:
            row('N14 restarts: target answers before the row starts', False, 'pb exec failed for 180 s')
            return
        time.sleep(5)
    name = fresh_name(); sessions.append(name)
    c1 = connect(a.target, name)
    try:
        mark = uuid.uuid4().hex[:12]
        c1.run(f'export PB_RESTART_MARK={mark}')
        c1.send("for i in $(seq 1 120); do echo RS$i; sleep 1; done\r")
        gap_keys = []
        for step, command in (('host runtime', a.restart_target_cmd), ('client daemon', a.restart_client_cmd)):
            time.sleep(10)
            t0 = time.monotonic()
            run_shell(command)
            print(f'  {step} restarted after {time.monotonic() - t0:.1f} s', flush=True)
            gap_keys.append(uuid.uuid4().hex)
            c1.send("printf '\\nPB_GAP_%s\\n' '" + gap_keys[-1] + "'\r")  # typed right after the restart
        t0 = time.monotonic()
        try:
            c1.expect('RS120', 240)
            row('N14 restarts: the terminal recovers on its own', True, f'{time.monotonic() - t0:.1f} s after the last restart')
        except RuntimeError:
            row('N14 restarts: the terminal recovers on its own', False, 'no output 240 s after the last restart')
            return
        c1.settle(3)
        reasons = re.findall(rb'Reconnect(?:ing: [^\]\r\n]*| ?ed after [^\]\r\n]*)', bytes(c1.output))
        # the client-daemon step must name itself, not read as a lost link
        row('N14 restarts: the terminal says why it reconnects and recovered', any(b'on this computer restarted' in r for r in reasons), b' | '.join(sorted(set(reasons)))[:200].decode(errors='replace'))
        ok, why = exactly_once(bytes(c1.output), b'RS', 1, 120)
        row('N14 restarts: output arrives exactly once', ok, why)
        counts = [bytes(c1.output).count(b'PB_GAP_' + k.encode()) for k in gap_keys]
        row('N14 restarts: keys typed right after each restart ran exactly once', counts == [1] * len(counts), f'{counts}')
        key = uuid.uuid4().hex
        c1.send("printf '\\nPB_KEY_%s_%s\\n' \"$PB_RESTART_MARK\" '" + key + "'\r")
        try:
            c1.expect('PB_KEY_' + mark + '_' + key, 60)
            row('N14 restarts: the same shell answers (its environment survived)', True)
        except RuntimeError:
            row('N14 restarts: the same shell answers (its environment survived)', False, 'no answer or a different shell')
    finally:
        c1.close()


def daemon_restart_typing(a, sessions):
    """N12 (F3-16): this machine's daemon restarts while output runs and keys are typed during the restart:
    nothing printed or typed is lost or repeated, and the same shell answers."""
    if not a.restart_client_cmd:
        row('N12 daemon restart while typing', None, 'needs --restart-client-cmd')
        return
    name = fresh_name(); sessions.append(name)
    c1 = connect(a.target, name)
    try:
        mark = uuid.uuid4().hex[:12]
        c1.run(f'export PB_RESTART_MARK={mark}')
        c1.send("for i in $(seq 1 60); do echo DR$i; sleep 1; done\r")
        time.sleep(5)
        restart = subprocess.Popen(a.restart_client_cmd, shell=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        key = uuid.uuid4().hex
        time.sleep(0.5)
        c1.send("printf '\\nPB_DKEY_%s\\n' '" + key + "'\r")  # typed while the daemon restarts
        restart.wait(120)
        try:
            c1.expect('DR60', 180)
        except RuntimeError:
            tail = re.sub(rb'DR[0-9]+\r?\n', b'', bytes(c1.output)[-2000:])[-300:]
            row('N12 daemon restart: the terminal recovers on its own', False, f'no output 180 s after the restart; client exited={c1.proc.poll() if hasattr(c1, "proc") else "?"} tail={tail!r}')
            return
        c1.settle(3)
        out = bytes(c1.output)
        ok, why = exactly_once(out, b'DR', 1, 60)
        row('N12 daemon restart: output arrives exactly once', ok, why)
        count = out.count(b'PB_DKEY_' + key.encode())
        row('N12 daemon restart: keys typed during it ran exactly once', count == 1, f'{count} times')
        probe = uuid.uuid4().hex
        c1.send("printf '\\nPB_SAME_%s_%s\\n' \"$PB_RESTART_MARK\" '" + probe + "'\r")
        try:
            c1.expect('PB_SAME_' + mark + '_' + probe, 60)
            row('N12 daemon restart: the same shell answers', True)
        except RuntimeError:
            row('N12 daemon restart: the same shell answers', False, 'no answer or a different shell')
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
        # The hardest real case at once: the foreground job floods output (yes) and never reads the
        # 256 KiB paste, which fills the tty queue, and the second viewer (c2) is stalled. The host
        # delivers Ctrl-C ahead of the stuck paste and discards what is left of it (F3-11).
        c1.send('yes\r')
        time.sleep(3)
        c1.send(('x' * 1023 + '\n') * 256)
        c1.send('\x03')
        # The tty discards input typed before the interrupt lands, so wait for yes to stop
        # (output goes quiet) before typing the next command.
        t0 = time.monotonic()
        stopped = c1.quiet_after(1.0, 30)
        if stopped is None:
            row('N6 Ctrl-C stops yes promptly', False, 'output still flowing 30 s after Ctrl-C')
        else:
            row('N6 Ctrl-C stops yes promptly', stopped < 5, f'output stopped {stopped:.1f} s after Ctrl-C')
        try:
            c1.run_discarding('true', 15)
            row('N6 the shell answers after Ctrl-C', True)
        except RuntimeError as error:
            row('N6 the shell answers after Ctrl-C', False, str(error))
        os.kill(c2.pid, signal.SIGCONT)
        t0 = time.monotonic()
        try:
            c2.run_discarding('true', 120)
            row('N6 the stalled window recovers after it resumes', True, f'{time.monotonic() - t0:.0f} s')
        except RuntimeError as error:
            row('N6 the stalled window recovers after it resumes', False, f'{error} ({time.monotonic() - t0:.0f} s)')
    finally:
        c1.close()
        c2.close()


def keystrokes(a, sessions):
    """No reconnect while typing: 2 000 single keystrokes, each its own write, cause no stream
    reopen (the daemon logs peer_stream_opened per stream) and the shell answers right after."""
    import glob

    def opens():
        files = glob.glob(str(Path.home() / '.local/state/paperboat/diagnostics/events-*.ndjson'))
        return sum(open(f).read().count('peer_stream_opened') for f in files)

    name = fresh_name(); sessions.append(name)
    c1 = connect(a.target, name)
    try:
        c1.send('cat > /dev/null\r')
        time.sleep(1)
        before = opens()
        for _ in range(2000):
            c1.send('x')
            time.sleep(0.01)
        c1.send('\r\x04')
        t0 = time.monotonic()
        c1.run_discarding('true', 60)
        reopened = opens() - before
        row('N11 2 000 keystrokes: no stream reopen', reopened == 0, f'{reopened} reopens')
        row('N11 the shell answers right after typing', time.monotonic() - t0 < 2, f'{time.monotonic() - t0:.1f} s')
    finally:
        c1.close()


def echo_latency(a, sessions):
    """Input-to-echo latency: 200 single keystrokes into cat (the tty echoes each), p50/p99 idle and
    while another session on the target floods output. Reported, and failed above 150 ms p99."""
    import select
    import statistics

    def sample(c, count=200):
        times = []
        for i in range(count):
            ch = 'abcdefghij'[i % 10].encode()
            c.pending.clear()
            t0 = time.monotonic()
            c.send(ch)
            deadline = t0 + 5
            got = b''
            while ch not in got and time.monotonic() < deadline:
                if select.select([c.fd], [], [], 0.05)[0]:
                    got += os.read(c.fd, 4096)
            times.append((time.monotonic() - t0) * 1000 if ch in got else 5000)
            time.sleep(0.02)
        q = statistics.quantiles(times, n=100)
        return q[49], q[98]

    name = fresh_name(); sessions.append(name)
    c1 = connect(a.target, name)
    flood = None
    try:
        c1.send('stty echo; cat\r')
        c1.settle(1)
        p50, p99 = sample(c1)
        row('N16 echo latency idle', p99 < 150, f'p50 {p50:.0f} ms, p99 {p99:.0f} ms')
        other = fresh_name(); sessions.append(other)
        flood = connect(a.target, other)
        flood.send('yes\r')
        time.sleep(2)
        p50, p99 = sample(c1)
        row('N16 echo latency while another session floods', p99 < 150, f'p50 {p50:.0f} ms, p99 {p99:.0f} ms')
    finally:
        if flood is not None:
            flood.send('\x03')
            flood.close()
        c1.close()


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


ROWS = {'two': two_clients, 'reconnect': reconnect_keeps_control, 'rejoin': leave_rejoin, 'sleep': sleep_wake, 'net': network_drop,
        'server': control_plane_down, 'blackhole': black_hole, 'restart': restarts, 'daemon': daemon_restart_typing, 'fast': fast_output, 'resize': resize_storm,
        'paste': large_paste, 'many': many_sessions, 'keys': keystrokes, 'latency': echo_latency, 'shared': shared}


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('target')
    p.add_argument('--only', default=','.join(ROWS))
    p.add_argument('--restart-target-cmd', default='', help='shell command that restarts the TARGET machine\'s host runtime (N14)')
    p.add_argument('--restart-client-cmd', default='', help="shell command that restarts this machine's daemon (N14)")
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
