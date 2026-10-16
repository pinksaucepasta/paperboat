#!/usr/bin/env python3
"""Qualify a named terminal's normal interactive behavior without printing its output."""
import argparse
import errno
import fcntl
import json
import os
from pathlib import Path
import pty
import select
import shlex
import signal
import struct
import sys
import termios
import tempfile
import time
import uuid

from pairing_lifecycle import command_diagnostic
from install_lifecycle import registration
from machine_lifecycle import machine, restore_all
from session_cleanup import cleanup


class Terminal:
    def __init__(self, arguments, cwd=None):
        self.arguments = list(map(str, arguments))
        self.output = bytearray()
        self.pending = bytearray()
        self.query_buffer = bytearray()
        self.status = None
        self.pid, self.fd = pty.fork()
        if self.pid == 0:
            if cwd is not None:
                os.chdir(cwd)
            environment = dict(os.environ, TERM='xterm-256color', LC_ALL='C')
            os.execvpe(self.arguments[0], self.arguments, environment)
        self.resize(24, 80)

    def resize(self, rows, columns):
        fcntl.ioctl(self.fd, termios.TIOCSWINSZ, struct.pack('HHHH', rows, columns, 0, 0))
        os.kill(self.pid, signal.SIGWINCH)

    def send(self, data):
        data = data.encode() if isinstance(data, str) else data
        deadline = time.monotonic() + 15
        while data:
            if time.monotonic() >= deadline:
                raise RuntimeError('terminal input exceeded its delivery bound')
            if select.select([], [self.fd], [], 0.1)[1]:
                count = os.write(self.fd, data[:1024])
                data = data[count:]

    def answer_queries(self, chunk):
        # This fixture's virtual terminal starts at home with white on black.
        # Answer capability probes rather than leaving TTY applications blocked.
        self.query_buffer.extend(chunk)
        requests = [(b'\x1b[6n', b'\x1b[1;1R')]
        for number, colour in [(b'10', b'ffff/ffff/ffff'), (b'11', b'0000/0000/0000')]:
            reply = b'\x1b]' + number + b';rgb:' + colour + b'\x07'
            for end in [b'\x07', b'\x1b\\']:
                requests.append((b'\x1b]' + number + b';?' + end, reply))
        while True:
            matches = [(self.query_buffer.find(request), request, reply)
                       for request, reply in requests if request in self.query_buffer]
            if not matches:
                break
            offset, request, reply = min(matches)
            del self.query_buffer[:offset + len(request)]
            self.send(reply)
        self.query_buffer = self.query_buffer[-16:]

    def expect(self, token, timeout=45):
        token = token.encode() if isinstance(token, str) else token
        deadline = time.monotonic() + timeout
        while token not in self.pending:
            if time.monotonic() >= deadline:
                raise RuntimeError('terminal did not produce the expected fixture output')
            if not select.select([self.fd], [], [], 0.1)[0]:
                continue
            try:
                chunk = os.read(self.fd, 65536)
            except OSError as error:
                if error.errno != errno.EIO:
                    raise
                chunk = b''
            if not chunk:
                raise RuntimeError('terminal closed before the expected fixture output')
            self.answer_queries(chunk)
            self.pending.extend(chunk)
            self.output.extend(chunk)
            if len(self.output) > 4 << 20:
                raise RuntimeError('terminal exceeded its output bound')
        end = self.pending.index(token) + len(token)
        observed = bytes(self.pending[:end])
        del self.pending[:end]
        return observed

    def marker(self, label, prefix=''):
        suffix = uuid.uuid4().hex
        token = 'PB_' + label + '_' + suffix
        self.send(prefix + "printf '\\nPB_" + label + "_%s\\n' '" + suffix + "'\r")
        return token

    def wait_exit(self, timeout=15):
        deadline = time.monotonic() + timeout
        while self.status is None:
            completed, status = os.waitpid(self.pid, os.WNOHANG)
            if completed:
                self.status = os.waitstatus_to_exitcode(status)
                # Reaping can win the race with reading the child's final error.
                while select.select([self.fd], [], [], 0)[0]:
                    try:
                        chunk = os.read(self.fd, 65536)
                    except OSError as error:
                        if error.errno != errno.EIO:
                            raise
                        break
                    if not chunk:
                        break
                    self.output.extend(chunk)
                    if len(self.output) > 4 << 20:
                        raise RuntimeError('terminal exceeded its output bound')
                break
            if time.monotonic() >= deadline:
                raise RuntimeError('terminal did not exit within its cleanup bound')
            if select.select([self.fd], [], [], 0.1)[0]:
                try:
                    chunk = os.read(self.fd, 65536)
                except OSError as error:
                    if error.errno != errno.EIO:
                        raise
                    chunk = b''
                self.answer_queries(chunk)
                self.output.extend(chunk)
                if len(self.output) > 4 << 20:
                    raise RuntimeError('terminal exceeded its output bound')
        return self.status

    def close(self):
        if self.fd is None:
            return
        try:
            if self.status is None:
                os.kill(self.pid, signal.SIGTERM)
                try:
                    self.wait_exit(5)
                except RuntimeError:
                    os.kill(self.pid, signal.SIGKILL)
                    _, status = os.waitpid(self.pid, 0)
                    self.status = os.waitstatus_to_exitcode(status)
        finally:
            os.close(self.fd)
            self.fd = None


def applications(terminal, monitor=None):
    suffix = uuid.uuid4().hex
    prompt = 'PB_APP_PROMPT_' + suffix + '> '
    terminal.send("PS1=$(printf 'PB_APP_PROMPT_%s> ' '" + suffix + "')\r")
    terminal.expect(prompt)
    editor = "vim -Nu NONE -n -i NONE -c " + shlex.quote("call setline(1, 'PB_VIM_'.'" + suffix + "')")
    terminal.send(editor + '\r')
    terminal.expect('PB_VIM_' + suffix)
    terminal.send(':q!\r')
    terminal.expect(prompt)
    terminal.expect(terminal.marker('EDITOR_EXIT'))
    terminal.send("i=1; while [ \"$i\" -le 100 ]; do printf 'PB_LESS_%03d\\n' \"$i\"; i=$((i+1)); done | LESS= LESSOPEN= less -R\r")
    terminal.expect('PB_LESS_001')
    terminal.send('G')
    terminal.expect('PB_LESS_100')
    terminal.send('q')
    terminal.expect(prompt)
    terminal.expect(terminal.marker('PAGER_EXIT'))
    if monitor is not None:
        terminal.send(shlex.quote(monitor) + ' --pid "$$"\r')
        terminal.expect('CPU%')
        terminal.expect('MEM%')
        terminal.send('q')
        terminal.expect(prompt)
        terminal.expect(terminal.marker('MONITOR_EXIT'))


def qualify(pb, target, check, monitor, entry='connect', cwd=None, status_bar='off'):
    name = 'smoke-terminal-' + uuid.uuid4().hex[:12]
    terminal = None
    started = time.monotonic()
    try:
        if entry == 'new':
            own = machine(pb, registration()['machine_id'])
            if target not in (own['id'], own['alias']):
                raise RuntimeError('pb new must be checked against this exact enrolled machine')
            arguments = [pb, 'new']
        else:
            arguments = [pb, target] if entry == 'root' else [pb, 'connect', target]
        terminal = Terminal([*arguments, '--name', name, '--status-bar', status_bar], cwd=cwd)
        terminal.expect(b'\x1b[?2004h')
        if status_bar == 'on':
            terminal.expect(b'\x1b[1;23r')
        terminal.expect(terminal.marker('READY', 'stty -echo; '))
        if entry == 'new':
            expected = shlex.quote(str(Path(cwd).resolve()))
            terminal.expect(terminal.marker('LOCAL_CWD', '[ "$(pwd -P)" = ' + expected + ' ] && '))
        if check not in ('paste', 'typing', 'apps', 'monitor'):
            terminal.send("printf '\\nPB_ARITH_%s\\n' \"$((6*7))\"; printf '\\nPB_UNICODE_λ船\\n'\r")
            terminal.expect('PB_ARITH_42')
            terminal.expect('PB_UNICODE_λ船')
            terminal.resize(37, 109)
            terminal.send("printf '\\nPB_SIZE_'; stty size\r")
            terminal.expect('PB_SIZE_' + ('36' if status_bar == 'on' else '37') + ' 109')
            terminal.send("printf '\\033[31mPB_COLOUR\\033[0m\\n'\r")
            terminal.expect(b'\x1b[31mPB_COLOUR\x1b[0m')
            for control, label in [(b'\x03', 'INTERRUPTED'), (b'\x1a', 'STOPPED')]:
                suffix = uuid.uuid4().hex
                ready = 'PB_FOREGROUND_' + suffix
                command = "printf '\\nPB_FOREGROUND_%s\\n' '" + suffix + "'; exec sleep 60"
                terminal.send('sh -c ' + shlex.quote(command) + '\r')
                terminal.expect(ready)
                terminal.send(control)
                if control == b'\x1a':
                    jobs = terminal.expect(terminal.marker('JOBS', 'jobs -s; '))
                    if b'Stopped' not in jobs and b'suspended' not in jobs.lower():
                        raise RuntimeError('Ctrl-Z did not stop the foreground fixture')
                    terminal.expect(terminal.marker(label, 'kill -KILL %1; wait %1 2>/dev/null; '))
                else:
                    terminal.expect(terminal.marker(label))
        if check in ('paste', 'typing', 'all'):
            payload = 'paste_' + 'a' * 8192
            command = "PB_PASTE='" + payload + "'; printf '\\nPB_PASTE_LENGTH_%s\\n' \"${#PB_PASTE}\""
            if check == 'typing':
                terminal.send(command + '\r')
            else:
                terminal.send(b'\x1b[200~' + command.encode() + b'\x1b[201~\r')
            terminal.expect('PB_PASTE_LENGTH_' + str(len(payload)))
        if check in ('main', 'apps', 'monitor', 'all'):
            applications(terminal, monitor if check in ('monitor', 'all') else None)
        terminal.expect(terminal.marker('DONE'))
        terminal.send(b'\x04')
        if terminal.wait_exit() != 0:
            raise RuntimeError('Ctrl-D did not end the terminal successfully')
        phases = ['input_output', 'unicode', 'resize', 'colour', 'ctrl_c', 'ctrl_z'] if check not in ('paste', 'typing', 'apps', 'monitor') else []
        if check in ('paste', 'typing', 'all'):
            phases.append('large_typing' if check == 'typing' else 'large_paste')
        if check in ('main', 'apps', 'monitor', 'all'):
            phases += ['vim', 'less']
        if check in ('monitor', 'all'):
            phases.append('htop')
        result = {'check': 'terminal-' + check, 'entry': entry, 'status_bar': status_bar, 'target': target, 'status': 'PASS',
                  'seconds': round(time.monotonic() - started, 1),
                  'phases': phases + ['ctrl_d', 'exact_session_cleanup']}
    except Exception as error:
        if terminal is not None:
            receipt = command_diagnostic(terminal.arguments, terminal.output.decode(errors='replace'), '')
            raise RuntimeError('terminal main path failed; private diagnostics: ' + str(receipt)) from error
        raise
    finally:
        restore_all(sys.exc_info()[1],
                    lambda: terminal.close() if terminal is not None else None,
                    lambda: cleanup(str(pb), target, name))
    print(json.dumps(result), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--check', choices=['main', 'paste', 'typing', 'apps', 'monitor', 'all'], default='all')
    parser.add_argument('--monitor-path', default='htop')
    parser.add_argument('--entry', choices=['connect', 'root', 'new'], default='connect')
    parser.add_argument('--cwd-root', type=Path, default=Path.home(),
                        help='parent of the distinct project directory; /tmp exercises namespace isolation')
    args = parser.parse_args()
    pb = Path.home() / '.local/bin/pb'
    if args.entry == 'new':
        with tempfile.TemporaryDirectory(prefix='.paperboat-terminal-cwd-', dir=args.cwd_root) as directory:
            qualify(pb, args.target, args.check, args.monitor_path, args.entry, directory)
    else:
        qualify(pb, args.target, args.check, args.monitor_path, args.entry)


if __name__ == '__main__':
    main()
