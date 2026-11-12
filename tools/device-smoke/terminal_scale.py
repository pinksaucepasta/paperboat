#!/usr/bin/env python3
"""F21-164 on real machines: one machine holds 100 concurrent terminals, one terminal 50 viewers.

Run on a client machine against a target of the same account:  terminal_scale.py TARGET [--terminals 100] [--viewers 50]
  1. opens N native terminals (`pb connect TARGET --name smoke-scale-*`) at once; every shell must answer (ready time p50/p95/max);
  2. all N echo a fresh marker at the same time, three rounds (echo round trip p50/p95/max);
  3. an output burst (about 27 MB) in 10 of them, the clients closed in the middle of it, the burst's sessions closed and
     deleted: closing must work within the bound while the others still echo;
  4. one terminal with V extra viewers (attached by name), one viewer stopped (SIGSTOP) while the shell floods output:
     the owner and the live viewers still receive their markers; the stopped viewer must not slow them;
  5. every session this run created is closed and deleted.
Prints one JSON receipt (numbers only; terminal contents are never kept or printed).
"""
import argparse
import json
import os
from pathlib import Path
import pty
import re
import select
import signal
import statistics
import struct
import subprocess
import sys
import termios
import fcntl
import time
import uuid

from session_cleanup import cleanup

PB = str(Path.home() / '.local/bin/pb')
READY_SEQ = b'\x1b[?2004h'  # the shell enabled bracketed paste: it is at its prompt
QUERIES = [(b'\x1b[6n', b'\x1b[1;1R'),
           (b'\x1b]10;?\x07', b'\x1b]10;rgb:ffff/ffff/ffff\x07'), (b'\x1b]11;?\x07', b'\x1b]11;rgb:0000/0000/0000\x07'),
           (b'\x1b]10;?\x1b\\', b'\x1b]10;rgb:ffff/ffff/ffff\x07'), (b'\x1b]11;?\x1b\\', b'\x1b]11;rgb:0000/0000/0000\x07')]
TAIL = 1 << 16


class Term:
    def __init__(self, target, extra):
        self.t0 = time.monotonic()
        self.pid, self.fd = pty.fork()
        if self.pid == 0:
            os.execve(PB, [PB, 'connect', target, *extra, '--status-bar', 'off'],
                      dict(os.environ, TERM='xterm-256color', LC_ALL='C'))
        fcntl.ioctl(self.fd, termios.TIOCSWINSZ, struct.pack('HHHH', 24, 80, 0, 0))
        self.tail = b''
        self.ready_at = None
        self.alive = True
        self.bytes = 0
        self.marks = {}
        self.done = {}

    def pump(self):
        """Read what is available (bounded), answer terminal queries, keep only a tail. False once the pty closed."""
        try:
            chunk = os.read(self.fd, 1 << 16)
        except OSError:
            chunk = b''
        if not chunk:
            self.alive = False
            return False
        self.bytes += len(chunk)
        self.tail = (self.tail + chunk)[-TAIL:]
        for query, reply in QUERIES:
            if query in chunk:
                os.write(self.fd, reply)
        if self.ready_at is None and READY_SEQ in self.tail:
            self.ready_at = time.monotonic() - self.t0
        for token, started in self.marks.items():
            if token not in self.done and token in self.tail:
                self.done[token] = time.monotonic() - started
        return True

    def send(self, text):
        os.write(self.fd, text.encode())

    def mark(self):
        """Send a command printing a fresh marker; the echoed command line does not contain the marker itself."""
        suffix = uuid.uuid4().hex[:16]
        self.send("printf 'PB_%s_%s\\n' SC " + suffix + '\r')
        token = ('PB_SC_' + suffix).encode()
        self.marks[token] = time.monotonic()
        return token

    def close(self):
        if self.pid:
            for sig in (signal.SIGCONT, signal.SIGTERM):
                try:
                    os.kill(self.pid, sig)
                except OSError:
                    pass
            deadline = time.monotonic() + 5
            while time.monotonic() < deadline:
                done, _ = os.waitpid(self.pid, os.WNOHANG)
                if done:
                    break
                time.sleep(0.05)
            else:
                os.kill(self.pid, signal.SIGKILL)
                os.waitpid(self.pid, 0)
            self.pid = 0
        if self.fd is not None:
            os.close(self.fd)
            self.fd = None


def pump_all(terms, seconds):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        live = [t for t in terms if t.fd is not None and t.alive]
        if not live:
            time.sleep(0.05)
            continue
        for fd in select.select([t.fd for t in live], [], [], 0.05)[0]:
            next(t for t in live if t.fd == fd).pump()


def wait_for(terms, condition, timeout):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        pump_all(terms, 0.1)
        if condition():
            return True
    return False


def quantiles(values):
    if not values:
        return None
    values = sorted(values)
    return {'p50': round(values[len(values) // 2], 3), 'p95': round(values[min(len(values) - 1, int(len(values) * 0.95))], 3),
            'max': round(values[-1], 3), 'n': len(values)}


def echo_round(terms, timeout=60):
    pending = {}
    for t in terms:
        pending[t] = t.mark()
    wait_for(terms, lambda: all(pending[t] in t.done for t in terms), timeout)
    got = [t.done[pending[t]] for t in terms if pending[t] in t.done]
    for t in terms:
        t.marks.clear()
        t.done.clear()
    return got, len(terms) - len(got)


def cleanup_all(target, names, workers=8):
    """Close and delete the named sessions, `workers` at a time (one at a time takes about 20 s each). Returns (done, seconds)."""
    from concurrent.futures import ThreadPoolExecutor
    def one(name):
        try:
            cleanup(PB, target, name)
            return 1
        except Exception:
            return 0
    started = time.monotonic()
    with ThreadPoolExecutor(workers) as pool:
        done = sum(pool.map(one, names))
    return done, round(time.monotonic() - started, 1)


def session_state(target, name):
    out = subprocess.run([PB, 'session', 'list', target, '--all', '--json'], capture_output=True, timeout=60).stdout
    return {s['name']: s for s in json.loads(out)['sessions']}.get(name)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--terminals', type=int, default=100)
    parser.add_argument('--viewers', type=int, default=50)
    parser.add_argument('--burst', type=int, default=10)
    parser.add_argument('--rate', type=float, default=3, help='terminals opened per second (a burst of 100 at once is a connect storm, not holding 100)')
    parser.add_argument('--flood-timeout', type=int, default=120)
    parser.add_argument('--flood-mb', type=int, default=3)
    parser.add_argument('--no-stall', action='store_true', help='control run: nobody is stopped')
    args = parser.parse_args()
    run = uuid.uuid4().hex[:6]
    names = [f'smoke-scale-{run}-{i:03d}' for i in range(args.terminals)]
    shared = f'smoke-scale-{run}-shared'
    terms, viewers, receipt = [], [], {'check': 'terminal-scale', 'target': args.target, 'terminals': args.terminals,
                                       'host_loadavg_start': open('/proc/loadavg').read().split()[:3]}
    try:
        for name in names:  # a person opens terminals one after another: --rate per second, not 100 at the same instant
            terms.append(Term(args.target, ['--name', name]))
            pump_all(terms, 1 / args.rate)
        ok = wait_for(terms, lambda: all(t.ready_at is not None for t in terms), 180)
        ready = [t.ready_at for t in terms if t.ready_at is not None]
        receipt['open'] = {'ready': len(ready), 'all_ready': ok, 'seconds': quantiles(ready)}
        stuck = {}
        for t in terms:  # what the terminals that never got a shell were saying (pb's own status line, never shell output)
            if t.ready_at is None:
                line = re.sub(rb'\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)', b'', t.tail).decode(errors='replace').strip().splitlines()
                key = re.sub(r'\d+', 'N', line[-1])[:120] if line else '(silent)'
                stuck[key] = stuck.get(key, 0) + 1
        receipt['open']['not_ready_last_line'] = stuck
        live = [t for t in terms if t.ready_at is not None]
        rounds, missing = [], 0
        for _ in range(3):
            got, lost = echo_round(live)
            rounds += got
            missing += lost
        receipt['echo'] = {'seconds': quantiles(rounds), 'missing': missing}
        # Output burst in `burst` terminals, closed in the middle of it, while the rest keep echoing.
        burst, rest = live[:args.burst], live[args.burst:]
        for t in burst:
            t.send('head -c 20000000 /dev/zero | base64\r')
        pump_all(terms, 1.0)
        started = time.monotonic()
        for t in burst:
            t.close()
        closed_clients = time.monotonic() - started
        got, lost = echo_round(rest)
        receipt['burst'] = {'closed': len(burst), 'client_close_seconds': round(closed_clients, 2),
                            'others_echo': quantiles(got), 'others_missing': lost}
        done, took = cleanup_all(args.target, names[:args.burst])
        receipt['burst']['session_cleanup_failures'] = args.burst - done
        receipt['burst']['session_cleanup_seconds'] = took
        # One terminal, V viewers, one stopped viewer while the shell floods.
        owner = Term(args.target, ['--name', shared])
        terms.append(owner)
        wait_for([owner], lambda: owner.ready_at is not None, 60)
        for _ in range(args.viewers):
            viewers.append(Term(args.target, ['--session', shared]))
        wait_for([owner, *viewers], lambda: all(v.bytes > 0 for v in viewers), 120)
        stalled = viewers[0]
        if not args.no_stall:
            os.kill(stalled.pid, signal.SIGSTOP)
        end_token = b'PB_FLOODEND_' + run.encode()
        owner.send("head -c %d /dev/zero | base64;" % (args.flood_mb << 20) + " printf '\\nPB_%s_%s\\n' FLOODEND " + run + '\r')
        flood_started = time.monotonic()
        survivors = [owner, *viewers[1:]]
        done_at = {}
        timeline = []
        while time.monotonic() - flood_started < args.flood_timeout and len(done_at) < len(survivors):
            pump_all(survivors, 1.0)
            for i, t in enumerate(survivors):
                if i not in done_at and end_token in t.tail:
                    done_at[i] = time.monotonic() - flood_started
            if len(timeline) < 60:
                timeline.append(round(owner.bytes / 1e6, 1))
        flood = {'owner_MB_cumulative_per_second': timeline[::5], 'finished': len(done_at), 'of': len(survivors),
                 'seconds_to_finish': quantiles(list(done_at.values()))}
        for t in survivors:
            t.tail = b''
        got, lost = echo_round(survivors, timeout=60)
        receipt['shared'] = {'viewers': len(viewers), 'stopped': 0 if args.no_stall else 1, 'flood': flood,
                             'echo_after_flood': quantiles(got), 'missing': lost}
        if not args.no_stall:
            os.kill(stalled.pid, signal.SIGCONT)
        pump_all([stalled], 3.0)
        receipt['shared']['stopped_viewer_alive_after_continue'] = stalled.alive
    finally:
        for t in [*terms, *viewers]:
            t.close()
        cleaned, took = cleanup_all(args.target, [*names[args.burst:], shared])
        receipt['final_cleanup'] = {'closed_and_deleted': cleaned, 'expected': len(names) - args.burst + 1, 'seconds': took}
    echo = receipt.get('echo', {}).get('seconds') or {}
    receipt['host_loadavg_end'] = open('/proc/loadavg').read().split()[:3]
    receipt['status'] = 'PASS' if (receipt['open']['all_ready'] and receipt['echo']['missing'] == 0
                                   and receipt['burst']['others_missing'] == 0
                                   and receipt['burst']['session_cleanup_failures'] == 0
                                   and receipt['shared']['missing'] == 0
                                   and receipt['shared']['flood']['finished'] == receipt['shared']['flood']['of']
                                   and receipt['final_cleanup']['closed_and_deleted'] == receipt['final_cleanup']['expected']
                                   and echo.get('p95', 99) < 5) else 'FAIL'
    print(json.dumps(receipt))
    return 0 if receipt['status'] == 'PASS' else 1


if __name__ == '__main__':
    sys.exit(main())
