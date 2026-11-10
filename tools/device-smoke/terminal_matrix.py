#!/usr/bin/env python3
"""Terminal quality matrix, native side: several real pb clients on one session under faults.

Runs on a signed-in client machine (the owner) against TARGET, another machine of the same
account. Every client is a real `pb connect` / `pb session attach` process under its own PTY.
Faults are injected locally: SIGKILL (a client leaves), SIGSTOP/SIGCONT (laptop sleep),
iptables (network loss, control plane unreachable; macOS: a pf anchor), resize storms, 50 MB of output and a
large paste. Each row prints PASS/FAIL/SKIP with a short reason; output content is never
printed. Only sessions named smoke-matrix-* are created, and all are closed and deleted.

A Windows machine on either side (PB_SMOKE_WINDOWS_PB set: the clients are pb.exe on the Windows device, each driven
through `ssh -tt`; a Windows TARGET: the session shell is cmd.exe under ConPTY, so rows type PowerShell one-liners):
rows whose fault or measurement has no equivalent there print SKIP with the reason.

Usage: terminal_matrix.py TARGET [--only ROW,ROW] [--member-cmd CMD [--member-pb PATH] --team TEAM --member ID]
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
from session_cleanup import cleanup, inventory  # noqa: E402
from session_lifecycle import selected  # noqa: E402
from terminal_lifecycle import Terminal  # noqa: E402
from network_drop import pf_block, pf_release  # noqa: E402
import windows_target  # noqa: E402
import winhost  # noqa: E402

PB = winhost.pb_path()
DEVICE = winhost.device()  # the Windows device the clients run on, else None: they run on this machine
TARGET = None  # set by main()
DARWIN = not DEVICE and os.uname().sysname == 'Darwin'
if DARWIN:
    RUNTIME = Path.home() / 'Library/Application Support/Paperboat/runtime'
    DIAGNOSTICS = Path.home() / 'Library/Application Support/Paperboat/state/diagnostics'
elif DEVICE:
    RUNTIME = DEVICE.runtime_dir()
    DIAGNOSTICS = None
else:
    RUNTIME = Path.home() / '.local/state/paperboat/runtime'
    DIAGNOSTICS = Path.home() / '.local/state/paperboat/diagnostics'
RESULTS = []


def row(name, ok, detail=''):
    state = 'SKIP' if ok is None else ('PASS' if ok else 'FAIL')
    RESULTS.append((name, state))
    print(f'{state} {name}' + (f': {detail}' if detail else ''), flush=True)


def win():
    """The sessions run on a Windows machine: cmd.exe under ConPTY, which echoes what is typed and has no sh tools."""
    return TARGET is not None and windows_target.is_windows(PB, TARGET)  # importers of connect() keep sh


def say(prefix, token):
    """The line to type that prints <prefix><token> on a line of its own."""
    if win():
        return windows_target.typed_marker(prefix + token)
    return "printf '\\n" + prefix + "%s\\n' '" + token + "'"


def say_mark(prefix, key):
    """Like say, with the shell's own PB_RESTART_MARK between prefix and key: only the same shell can print it."""
    if win():
        return windows_target.typed_marker(prefix, "+$env:PB_RESTART_MARK+'_%s'" % key)
    return "printf '\\n" + prefix + "%s_%s\\n' \"$PB_RESTART_MARK\" '" + key + "'"


def set_mark(mark):
    return f'set "PB_RESTART_MARK={mark}"' if win() else f'export PB_RESTART_MARK={mark}'


def count(prefix, last, slow=False):
    """The command that prints <prefix>1 .. <prefix>last, one per line; slow: one a second."""
    if win():
        return windows_target.count_line(prefix, last, slow)
    if slow:
        return 'for i in $(seq 1 %d); do echo %s$i; sleep 1; done' % (last, prefix)
    return "seq -f '%s%%g' 1 %d" % (prefix, last)


def noop():
    return '' if win() else 'true'


def pb_run(*arguments, timeout=60, check=False):
    """pb to completion with text output, on the machine the clients run on."""
    if DEVICE:
        result = PB.run_remote(arguments, timeout=timeout)
    else:
        result = subprocess.run([str(PB), *arguments], capture_output=True, text=True, timeout=timeout)
    if check and result.returncode:
        raise subprocess.CalledProcessError(result.returncode, [str(PB), *arguments])
    return result


class Client(Terminal):
    """One attached pb client. ready() waits for a shell that answers a marker."""

    key = None  # the name or session id on its command line: finds pb.exe on the Windows device

    def ready(self, timeout=90):
        if win():  # cmd.exe announces no bracketed paste and has no stty; its console always echoes
            token = 'PB_READY_' + uuid.uuid4().hex
            self.send(windows_target.typed_marker(token) + '\r')
            # A new ConPTY session is silent for about 12 s on a hosted runner and the Enter typed into it can be
            # lost while the typed text stays at the prompt: press Enter again until the marker comes back (an
            # extra Enter at a prompt only runs an empty line).
            deadline = time.monotonic() + timeout
            while True:
                try:
                    self.expect(token, min(5, max(1, deadline - time.monotonic())))
                    return
                except RuntimeError as error:
                    if 'closed' in str(error) or time.monotonic() >= deadline:
                        raise
                    self.send('\r')
        # Keys typed before the shell enables bracketed paste are lost by its line editor.
        self.expect(b'\x1b[?2004h', timeout)
        self.expect(self.marker('READY', 'stty -echo; '), timeout)

    def run(self, command, timeout=60):
        """Run command in the shell and wait until it has finished."""
        token = uuid.uuid4().hex
        self.send((command + (' & ' if win() else '; ') if command or not win() else '') + say('PB_DONE_', token) + '\r')
        return self.expect('PB_DONE_' + token, timeout)

    def device_pid(self):
        found = [p['ProcessId'] for p in DEVICE.processes(re.escape(self.key)) if p['Name'].lower() == 'pb.exe']
        if len(found) != 1:
            raise RuntimeError('expected one pb.exe for this client on the device, found %d' % len(found))
        return found[0]

    def kill(self):
        """The client dies without a goodbye."""
        if DEVICE:
            DEVICE.stop(self.device_pid())
        else:
            os.kill(self.pid, signal.SIGKILL)

    def suspend(self):
        """The client stops running (a sleeping laptop, a stalled window)."""
        if DEVICE:
            DEVICE.suspend(self.device_pid())
        else:
            os.kill(self.pid, signal.SIGSTOP)

    def resume(self):
        if DEVICE:
            DEVICE.resume(self.device_pid())
        else:
            os.kill(self.pid, signal.SIGCONT)

    def run_discarding(self, command, timeout):
        """Like run, for output too large to keep: only a sliding tail is kept to find the marker."""
        import select
        token = ('PB_DONE_' + uuid.uuid4().hex).encode()
        self.send((command + (' & ' if win() else '; ') if command or not win() else '') + say('PB_DONE_', token.decode()[8:]) + '\r')
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
        if win():  # read by typing in this window, which also makes it the active one
            found = re.findall(rb'PB_SIZE (\d+) (\d+)', self.run(windows_target.size_line()))
            return tuple(map(int, found[-1])) if found else None
        # marker-delimited, as terminal_lifecycle.unix_phases reads the size: a Windows client's ConPTY reflows the screen after a resize storm
        # (the answer can be split or overwritten by repaint sequences), so the pair is read behind its own marker, not at a line end
        out = self.run("printf '\\nPB_SIZE_'; stty size")
        out = re.sub(rb'\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[78=>DEHMNOPVWXZ\\^_c]|\x1b[ -/]+[0-~]', b'', out)
        found = re.findall(rb'PB_SIZE_(\d+) (\d+)', out)
        return tuple(map(int, found[-1])) if found else None


def connect(target, name, rows=24, cols=80):
    c = Client(winhost.terminal_argv(PB, ['connect', target, '--name', name, '--status-bar', 'off']), env=winhost.terminal_env(PB))
    c.key = name
    c.resize(rows, cols)
    c.ready()
    return c


def attach(target, session, rows=24, cols=80, typing=True):
    c = Client(winhost.terminal_argv(PB, ['connect', target, '--session', session, '--status-bar', 'off']), env=winhost.terminal_env(PB))
    c.key = session
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
    # A Windows client's ConPTY repaints around a burst and can glue sequences of any kind before or after a line: CSI with any final byte
    # (also ~), OSC, two-byte ESC sequences, and bare CRs. Strip those, then keep the strict one-line-per-number match (no substring search:
    # that would hide a lost or doubled line).
    out = re.sub(rb'\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[78=>DEHMNOPVWXZ\\^_c]|\x1b[ -/]+[0-~]', b'', out)
    out = re.sub(rb'\r+(?!\n)', b'', out).replace(b'\r\r\n', b'\r\n')
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
    runs = []  # the order lines arrived in, as consecutive runs: "1-6, 7-72, 7-90" shows which stretch came twice
    for n in got:
        if runs and runs[-1][1] + 1 == n:
            runs[-1][1] = n
        else:
            runs.append([n, n])
    order = ', '.join(f'{a}-{b}' if a != b else str(a) for a, b in runs[:12])
    return False, f'{len(got)} lines, {dup} duplicated, {missing} missing' + (f' (first missing: {gone})' if gone else '') + f' [arrived as {order}]'


def has_iptables():
    """Whether this controller (or, for the Windows device, the device's firewall) can cut the clients' network."""
    if DEVICE:
        return True
    return subprocess.run(['sudo', '-n', 'pfctl', '-s', 'info'] if DARWIN else ['sudo', '-n', 'iptables', '-S'],
                          capture_output=True).returncode == 0


NO_FIREWALL = ('no passwordless sudo pfctl on this machine' if DARWIN else 'no iptables on this machine')


def iptables(*rule):
    subprocess.run(['sudo', '-n', 'iptables', *rule], check=True, capture_output=True)


def drop_all(seconds):
    """Silently drop every non-loopback packet in both directions; drop_restore undoes it."""
    if DEVICE:  # pb.exe's own traffic only: the SSH that drives the device keeps working
        return DEVICE.cut_network(seconds + 60)
    if DARWIN:
        return pf_block('block drop quick on ! lo0 all\n', seconds + 60)
    iptables('-I', 'OUTPUT', '-o', 'lo', '-j', 'ACCEPT')
    iptables('-I', 'OUTPUT', '2', '-j', 'DROP')
    iptables('-I', 'INPUT', '-i', 'lo', '-j', 'ACCEPT')
    iptables('-I', 'INPUT', '2', '-j', 'DROP')


def drop_restore(block):
    if DEVICE:
        return block.release()
    if DARWIN:
        return pf_release(block)
    for chain, iface in [('OUTPUT', '-o'), ('INPUT', '-i')]:
        iptables('-D', chain, '-j', 'DROP')
        iptables('-D', chain, iface, 'lo', '-j', 'ACCEPT')


# Rows. Each takes (args, sessions) and records its own result; sessions collects names to clean up.

_TARGET_OS = {}


def target_darwin(target):
    """True when the machine the sessions run on is a Mac (its tools are BSD: stty -f, shasum)."""
    if target not in _TARGET_OS:
        _TARGET_OS[target] = not win() and 'Darwin' in pb_run('exec', target, '--', 'uname', '-s').stdout
    return _TARGET_OS[target]


def pty_size(target, tty):
    """The session's PTY size read from outside it (pb exec), so measuring never counts as activity."""
    out = pb_run('exec', target, '--', 'stty', '-f' if target_darwin(target) else '-F', tty, 'size').stdout
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


def session_size(target, session_id, want=None, seconds=10):
    """The session's live PTY size (rows, columns) from `pb session list --json`: passive, so it also works for a
    ConPTY session, which has no tty to ask from outside and where typing a query would make that window active."""
    deadline = time.monotonic() + seconds
    while True:
        match = [s for s in inventory(PB, target)[1] if s.get('id') == session_id]
        size = (match[0]['rows'], match[0]['columns']) if len(match) == 1 and match[0].get('columns') and match[0].get('rows') else None
        if want is None or size == want or time.monotonic() >= deadline:
            return size
        time.sleep(0.5)


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
            c2.send(say('PB_BOTH_', token) + '\r')
            c2.expect('PB_BOTH_' + token)
            seen = True
            try:
                c1.expect('PB_BOTH_' + token, 20)
            except RuntimeError:
                seen = False
            row('N1 two windows: output typed in one shows in the other', seen)
            if win():  # each window reads the size by typing, which is what makes it the active one
                size = c2.size()
                row('N1 two windows: the size is the window that typed last (window 2)', size == (24, 80), f'{size}')
                size = c1.size()
                row('N1 two windows: typing moves the size to that window (window 1)', size == (30, 100), f'{size}')
                c2.resize(20, 70)
                time.sleep(2)
                size = session_size(a.target, s['id'])
                row('N1 two windows: the other window resizing does not change it', size == (30, 100), f'{size}')
                c1.resize(40, 120)
                size = session_size(a.target, s['id'], (40, 120))
                row('N1 two windows: the active window resizing does', size == (40, 120), f'{size}')
                return
            tty = session_tty(c2)  # typing in window 2: it is the latest active
            size = settled_size(a.target, tty, (24, 80))
            row('N1 two windows: the size is the window that typed last (window 2)', size == (24, 80), f'{size}')
            c1.run(noop())
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
            if win():  # window 1 is in use (it typed last)
                c1.size()
                before = session_size(a.target, s['id'], (30, 100))
            else:
                tty = session_tty(c1)
                before = settled_size(a.target, tty, (30, 100))
            c2.close()
            c2 = attach(a.target, s['id'], 24, 80, typing=False)  # window 2 reconnects without typing
            time.sleep(3)
            after = session_size(a.target, s['id']) if win() else pty_size(a.target, tty)
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
        c1.kill()
        c1.close()
        c2.run(count('LR', 2000), 60)
        c1 = attach(a.target, s['id'])
        c1.settle(2)
        # A fresh client joins at the live edge with a redraw and recent scrollback (as tmux does),
        # so the replay is a tail: it must be contiguous, end at the newest line and repeat nothing.
        got = [int(n) for n in lines(bytes(c1.output), b'LR')]
        ok = bool(got) and got[-1] == 2000 and got == list(range(got[0], 2001))
        row('N2 rejoin: replay is a contiguous tail ending at the newest line, nothing repeated', ok,
            f'{len(got)} of 2000 lines shown' if got else 'no lines')
        c1.run(noop())
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
        c1.suspend()
        c2.run(count('SW', 90, slow=True), 150)
        key = uuid.uuid4().hex
        c1.send(say('PB_KEY_', key) + '\r')  # sits in c1's PTY while it sleeps
        c1.resume()
        t0 = time.monotonic()
        try:
            c1.expect('PB_KEY_' + key, 120)
            row('N3 wake: keys typed during sleep arrive', True, f'{time.monotonic() - t0:.1f} s after wake')
        except RuntimeError:
            row('N3 wake: keys typed during sleep arrive', False, 'not echoed within 120 s')
        c2.settle(2)
        runs = bytes(c2.output).count(b'PB_KEY_' + key.encode())
        row('N3 wake: those keys ran exactly once', runs == 1, f'{runs} times')
        ok, why = exactly_once(bytes(c1.output), b'SW', 1, 90)
        row('N3 wake: output made during sleep is replayed exactly once', ok, why)
    finally:
        c1.close()
        c2.close()


def network_drop(a, sessions):
    """W6/W8: the client's network drops for 45 s while output continues on the host."""
    if not has_iptables():
        row('N4 network drop: session resumes, nothing lost or doubled', None, NO_FIREWALL)
        return
    name = fresh_name(); sessions.append(name)
    c1 = connect(a.target, name)
    try:
        c1.send(count('ND', 60, slow=True) + '\r')
        time.sleep(5)
        block = drop_all(45)
        try:
            time.sleep(45)
        finally:
            drop_restore(block)
        t0 = time.monotonic()
        try:
            c1.expect('ND60', 120)
            row('N4 network drop: session resumes on its own', True, f'{time.monotonic() - t0:.1f} s after the network returned')
        except RuntimeError:
            seen = sorted({int(n) for n in re.findall(rb'ND(\d+)', bytes(c1.output))})  # the test's own markers only
            try:
                done, code = os.waitpid(c1.pid, os.WNOHANG)
                client = f'pb exited {os.waitstatus_to_exitcode(code)}' if done else 'pb still running'
            except ChildProcessError:
                client = 'pb already reaped'
            plain = re.sub(rb'\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07', b'', bytes(c1.output)).decode('latin1')
            notes = [line.strip()[:100] for line in plain.splitlines() if line.strip() and not re.fullmatch(r'ND\d+', line.strip())][-3:]  # the client's own notice, not session data
            row('N4 network drop: session resumes on its own', False, f'no output 120 s after the network returned (ND markers seen: {len(seen)}, highest {seen[-1] if seen else None}; {client}; last lines {notes})')
            return
        c1.settle(2)
        ok, why = exactly_once(bytes(c1.output), b'ND', 1, 60)
        row('N4 network drop: output during the gap arrives exactly once', ok, why)
    finally:
        c1.close()


def control_plane_down(a, sessions):
    """W7: the server is unreachable (deploy, outage) mid-session; the open terminal keeps working."""
    if not has_iptables():
        row('N5 server unreachable: open terminal keeps working', None, NO_FIREWALL)
        return
    import json
    import socket
    from urllib.parse import urlparse
    host = urlparse(json.loads((RUNTIME / 'machine-registration.json').read_text())['server_url']).hostname
    addrs = sorted({ai[4][0] for ai in socket.getaddrinfo(host, 443, socket.AF_INET)})
    name = fresh_name(); sessions.append(name)
    c1 = connect(a.target, name)
    try:
        if DEVICE:
            block = DEVICE.cut_network(240, addresses=addrs, inbound=False)
        elif DARWIN:
            block = pf_block('block drop out quick to { %s }\n' % ' '.join(addrs), 240)
        else:
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
            if DEVICE:
                block.release()
            elif DARWIN:
                pf_release(block)
            else:
                for ip in addrs:
                    iptables('-D', 'OUTPUT', '-d', ip, '-j', 'DROP')
    finally:
        c1.close()


def black_hole(a, sessions):
    """N13 (F3-15): the network silently drops every packet for 60 s (no reset, no link-down). Output continues on
    the host and a command is typed meanwhile: it resumes on its own, nothing lost or doubled, the keys run once."""
    if not has_iptables():
        row('N13 black hole: session resumes, nothing lost or doubled', None, NO_FIREWALL)
        return
    name = fresh_name(); sessions.append(name)
    c1 = connect(a.target, name)
    try:
        c1.send(count('BH', 90, slow=True) + '\r')
        time.sleep(5)
        block = drop_all(60)
        key = uuid.uuid4().hex
        dropped, seen, detected, typed = time.monotonic(), len(c1.output), None, False
        try:
            while time.monotonic() - dropped < 60:
                c1.settle(0.5)  # reads the pty: output is only collected while reading
                if detected is None and b'reconnect' in bytes(c1.output)[seen:].lower():
                    detected = time.monotonic() - dropped
                if not typed and time.monotonic() - dropped >= 20:
                    c1.send(say('PB_KEY_', key) + '\r')  # typed into the black hole
                    typed = True
        finally:
            drop_restore(block)
        row('N13 black hole 60 s: the client shows it is reconnecting within 30 s', detected is not None and detected < 30,
            f'{detected:.1f} s' if detected is not None else 'never shown while the network was down')
        t0, size = time.monotonic(), len(c1.output)
        try:
            # BH90 is printed 90 s after the loop started, 25 s after the network returned whatever the client does, so
            # it only proves completion; the first output after the return is when the link resumed.
            while len(c1.output) == size and time.monotonic() - t0 < 150:
                c1.settle(0.5)
            first = time.monotonic() - t0
            c1.expect('BH90', 150)
            row('N13 black hole 60 s: session resumes on its own', first < 150, f'first output {first:.1f} s after the network returned')
        except RuntimeError:
            row('N13 black hole 60 s: session resumes on its own', False, 'no output 150 s after the network returned')
            return
        c1.settle(3)
        out = bytes(c1.output)
        ok, why = exactly_once(out, b'BH', 1, 90)
        row('N13 black hole 60 s: output during the gap arrives exactly once', ok, why)
        runs = out.count(b'PB_KEY_' + key.encode())
        row('N13 black hole 60 s: keys typed meanwhile ran exactly once', runs == 1, f'{runs} times')
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
    while pb_run('exec', a.target, '--', *(['cmd', '/c', 'exit', '0'] if win() else ['true'])).returncode != 0:
        if time.monotonic() > deadline:
            row('N14 restarts: target answers before the row starts', False, 'pb exec failed for 180 s')
            return
        time.sleep(5)
    name = fresh_name(); sessions.append(name)
    c1 = connect(a.target, name)
    try:
        mark = uuid.uuid4().hex[:12]
        c1.run(set_mark(mark))
        c1.send(count('RS', 120, slow=True) + '\r')
        gap_keys = []
        for step, command in (('host runtime', a.restart_target_cmd), ('client daemon', a.restart_client_cmd)):
            time.sleep(10)
            t0 = time.monotonic()
            run_shell(command)
            print(f'  {step} restarted after {time.monotonic() - t0:.1f} s', flush=True)
            gap_keys.append(uuid.uuid4().hex)
            c1.send(say('PB_GAP_', gap_keys[-1]) + '\r')  # typed right after the restart
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
        c1.send(say_mark('PB_KEY_', key) + '\r')
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
        c1.run(set_mark(mark))
        c1.send(count('DR', 60, slow=True) + '\r')
        time.sleep(5)
        restart = subprocess.Popen(a.restart_client_cmd, shell=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        key = uuid.uuid4().hex
        time.sleep(0.5)
        c1.send(say('PB_DKEY_', key) + '\r')  # typed while the daemon restarts
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
        runs = out.count(b'PB_DKEY_' + key.encode())
        row('N12 daemon restart: keys typed during it ran exactly once', runs == 1, f'{runs} times')
        probe = uuid.uuid4().hex
        c1.send(say_mark('PB_SAME_', probe) + '\r')
        try:
            c1.expect('PB_SAME_' + mark + '_' + probe, 60)
            row('N12 daemon restart: the same shell answers', True)
        except RuntimeError:
            row('N12 daemon restart: the same shell answers', False, 'no answer or a different shell')
    finally:
        c1.close()


class YesWatcher:
    """Sees on the target when the flood's `yes` ends, in a background `pb exec` (its own path, not the terminal under test): the target's
    clock at that moment (0.1 s resolution), so the stop time does not depend on how slowly a Windows client's ssh -tt hop draws the buffered
    output. It compares the target's clock with this machine's: both are NTP-synchronised hosts; an offset of a second would show in other rows."""

    def __init__(self, target, baseline):
        import threading
        self.end = None
        script = 'while [ "$(pgrep -x -c yes)" -gt %d ]; do sleep 0.1; done; python3 -c "import time;print(time.time())"' % baseline
        self.thread = threading.Thread(target=self._run, args=(target, script), daemon=True)
        self.thread.start()

    def _run(self, target, script):
        try:
            out = winhost.run_pb(PB, ['exec', target, '--', 'sh', '-c', script], 90).stdout.decode(errors='replace').strip()
            self.end = float(out.splitlines()[-1])
        except (ValueError, IndexError, RuntimeError, OSError, subprocess.SubprocessError):
            self.end = None

    def gone_at(self, timeout):
        self.thread.join(timeout)
        return self.end


def host_yes(target):
    """How many `yes` processes run on the target (a Windows client's view of whether the flood producer is gone)."""
    result = winhost.run_pb(PB, ['exec', target, '--', 'sh', '-c', 'pgrep -x -c yes || true'], 60)
    return int(result.stdout.decode(errors='replace').strip().splitlines()[-1] or 0)


def fast_output(a, sessions):
    """W9: 50 MB of output with a stalled second window; the first stays live, Ctrl-C works."""
    name = fresh_name(); sessions.append(name)
    c1 = connect(a.target, name)
    _, s = selected(PB, a.target, name)
    c2 = attach(a.target, s['id'])
    try:
        c2.suspend()  # a stalled viewer must not stall others
        t0 = time.monotonic()
        try:
            c1.run_discarding(windows_target.flood_line(1000000) if win()
                              else 'yes 0123456789012345678901234567890123456789012345678 | head -c 50000000', 300)
            row('N6 50 MB output finishes with a stalled second window', True, f'{time.monotonic() - t0:.0f} s')
        except RuntimeError:
            row('N6 50 MB output finishes with a stalled second window', False, 'not done in 300 s')
        c1.pending.clear()
        # The hardest real case at once: the foreground job floods output (yes) and never reads the
        # 256 KiB paste, which fills the tty queue, and the second viewer (c2) is stalled. The host
        # delivers Ctrl-C ahead of the stuck paste and discards what is left of it (F3-11).
        host_side = bool(DEVICE) and not win()  # a Windows client's ssh -tt + ConPTY hop buffers seconds of flood: see host_yes
        yes_before = host_yes(a.target) if host_side else 0
        c1.send((windows_target.flood_line() if win() else 'yes') + '\r')
        time.sleep(3)
        flooding = host_yes(a.target) > yes_before if host_side else len(c1.pending) > 0
        row('N6 the flood is running before Ctrl-C', flooding, 'yes on the target' if host_side else 'output arriving')
        watcher = YesWatcher(a.target, yes_before) if host_side else None
        c1.send(('x' * 1023 + '\n') * 256)
        c1.send('\x03')
        sent = time.time()
        if host_side:  # the producer must stop promptly; what the hop already buffered may take longer to reach the screen
            gone = watcher.gone_at(60)
            if gone is None:
                row('N6 Ctrl-C stops yes promptly', False, 'yes still running on the target 60 s after Ctrl-C')
            else:
                row('N6 Ctrl-C stops yes promptly', gone - sent < 5, f'yes on the target ended {gone - sent:.1f} s after Ctrl-C (target clock)')
        # The tty discards input typed before the interrupt lands, so wait for yes to stop
        # (output goes quiet) before typing the next command.
        stopped = c1.quiet_after(1.0, 60 if host_side else 30)
        label = 'N6 the output stops after Ctrl-C' if host_side else 'N6 Ctrl-C stops yes promptly'
        if stopped is None:
            row(label, False, 'output still flowing after Ctrl-C')
        else:  # the 5 s bound is the unix client's; a Windows client's screen only has to go quiet (the producer's stop is the row above)
            row(label, True if host_side else stopped < 5, f'output stopped {stopped:.1f} s after Ctrl-C')
        try:
            c1.run_discarding(noop(), 15)
            row('N6 the shell answers after Ctrl-C', True)
        except RuntimeError as error:
            row('N6 the shell answers after Ctrl-C', False, str(error))
        c2.resume()
        t0 = time.monotonic()
        try:
            c2.run_discarding(noop(), 120)
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
        if DEVICE:  # %LOCALAPPDATA%\Paperboat holds runtime/ and state/diagnostics/; an empty count would pass vacuously
            where = DEVICE.runtime_dir().parent / 'state' / 'diagnostics'
            if not DEVICE.files(where, 'events-*.ndjson'):
                raise RuntimeError('no daemon diagnostics on the device at %s: the reopen count would be meaningless' % where)
            return DEVICE.count_in_files(where, 'events-*.ndjson', 'peer_stream_opened')
        files = glob.glob(str(DIAGNOSTICS / 'events-*.ndjson'))
        return sum(open(f).read().count('peer_stream_opened') for f in files)

    name = fresh_name(); sessions.append(name)
    c1 = connect(a.target, name)
    try:
        c1.send((windows_target.sink_line() if win() else 'cat > /dev/null') + '\r')
        time.sleep(1)
        before = opens()
        for index in range(2000):
            # a Windows console line holds about 250 characters: Enter every 100 keys keeps each key accepted
            c1.send('\r' if win() and index % 100 == 99 else 'x')
            time.sleep(0.01)
        c1.send('\r\x1a\r' if win() else '\r\x04')  # end of input: Ctrl-Z Enter on a Windows console
        t0 = time.monotonic()
        c1.run_discarding(noop(), 60)
        reopened = opens() - before
        row('N11 2 000 keystrokes: no stream reopen', reopened == 0, f'{reopened} reopens')
        # A Windows client reaches the target through the controller's ssh -tt and the device's ConPTY: about 12 ms per key against the 10 ms the
        # keys are sent at (measured idle: marker round trip 0.5 s, 2000 keys then marker 0.6 s; matrix runs 4.6, 4.7 and 5.1 s), so up to ~5 s of
        # the typing queue drains after the last key. The zero-reopen row above is the starvation proof; the bound is 10 s there, 2 s elsewhere.
        bound = 10 if DEVICE else 2
        row('N11 the shell answers right after typing', time.monotonic() - t0 < bound, f'{time.monotonic() - t0:.1f} s (bound {bound} s)')
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
            if win() and i % 100 == 99:
                c.send('\r')  # a Windows console line holds about 250 characters
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

    if DEVICE:
        row('N16 echo latency', None, 'every key crosses the SSH hop from this controller to the Windows device: not the product latency')
        return
    name = fresh_name(); sessions.append(name)
    c1 = connect(a.target, name)
    flood = None
    try:
        c1.send((windows_target.sink_line() if win() else 'stty echo; cat') + '\r')
        c1.settle(1)
        p50, p99 = sample(c1)
        row('N16 echo latency idle', p99 < 150, f'p50 {p50:.0f} ms, p99 {p99:.0f} ms')
        other = fresh_name(); sessions.append(other)
        flood = connect(a.target, other)
        flood.send((windows_target.flood_line() if win() else 'yes') + '\r')
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
    if win():
        row('N8 256 KiB paste arrives byte for byte', None, 'cmd.exe has no raw input mode (stty raw) and no bracketed paste')
        return
    name = fresh_name(); sessions.append(name)
    c1 = connect(a.target, name)
    try:
        body = (uuid.uuid4().hex * 8 + '\n') * 1000  # 257,000 bytes of text lines
        want = hashlib.sha256(body.encode()).hexdigest()
        path = '/tmp/' + name + '.paste'
        c1.send('stty raw -echo; head -c ' + str(len(body)) + ' > ' + path + '; stty sane -echo\r')
        time.sleep(1)
        c1.send(body)
        digest = 'shasum -a 256 ' if target_darwin(a.target) else 'sha256sum '
        out = c1.run(digest + path + '; rm -f ' + path, 120)
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
                c.run(noop(), 30)
                working += 1
            except RuntimeError:
                pass
        row('N9 20 sessions open at once all work', failed == 0 and working == 20, f'{working}/20 working')
    finally:
        for c in clients:
            c.close()


def member(a, args, timeout=60):
    """Run pb as the team member on the member machine (MEMBER_CMD prefixes the remote command)."""
    remote = ' '.join(shlex.quote(x) for x in ['timeout', str(timeout), a.member_pb, *args])
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
        pb_run(*share, '--role', 'interactive', '--json', check=True)
        joined = Client(shlex.split(a.member_cmd) + [a.member_pb, 'session', 'join', s['id']])
        token = uuid.uuid4().hex
        joined.send(say('PB_MEMBER_', token) + '\r')
        try:
            c1.expect('PB_MEMBER_' + token, 60)
            row('N10 interactive member: their typing reaches the owner', True)
        except RuntimeError:
            row('N10 interactive member: their typing reaches the owner', False, 'not seen by the owner in 60 s')
        joined.close()
        pb_run(*share, '--role', 'viewer', '--json', check=True)
        joined = Client(shlex.split(a.member_cmd) + [a.member_pb, 'session', 'join', s['id']])
        time.sleep(5)
        token = uuid.uuid4().hex
        joined.send(say('PB_VIEWER_', token) + '\r')
        c1.run(windows_target.typed_line('Start-Sleep 5') if win() else 'sleep 5', 30)
        row('N10 viewer: their typing does not reach the shell',
            b'PB_VIEWER_' + token.encode() not in bytes(c1.output))
        live = uuid.uuid4().hex
        c1.send(say('PB_LIVE_', live) + '\r')
        try:
            joined.expect('PB_LIVE_' + live, 30)
            row('N10 viewer: sees the owner\'s live output', True)
        except RuntimeError:
            row('N10 viewer: sees the owner\'s live output', False)
        pb_run('--workspace', 'personal', 'session', 'unshare', s['id'], '--json', check=True)
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
    p.add_argument('--member-pb', default='/home/pbt/.local/bin/pb', help="the member's pb path")
    p.add_argument('--team', default='')
    p.add_argument('--member', default='', help='member account ID')
    a = p.parse_args()
    global TARGET
    TARGET = a.target
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
                cleanup(PB, a.target, name)
            except Exception as error:
                print(f'cleanup {name}: {error}', file=sys.stderr)
    failed = [n for n, s in RESULTS if s == 'FAIL']
    print('RESULT', 'PASS' if not failed else 'FAIL ' + str(len(failed)))
    if DEVICE:  # windows_checks.py reads one JSON status line; rows that all SKIP are not a pass
        import json
        states = [s for _, s in RESULTS]
        print(json.dumps({'check': 'terminal-matrix', 'target': a.target, 'status': 'PASS' if not failed and 'PASS' in states else 'FAIL',
                          'passed': states.count('PASS'), 'failed': len(failed), 'skipped': states.count('SKIP')}))
    return 1 if failed else 0


if __name__ == '__main__':
    sys.exit(main())
