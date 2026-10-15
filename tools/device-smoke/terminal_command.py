"""Run a device-owned install command with its real controlling terminal."""
import errno
import os
import pty
import select
import signal
import subprocess
import termios
import time


def run(argv, *, password, env=None, timeout=180):
    pid, terminal = pty.fork()
    if pid == 0:
        try:
            settings = termios.tcgetattr(0)
            settings[3] &= ~termios.ECHO
            termios.tcsetattr(0, termios.TCSANOW, settings)
            os.execvpe(str(argv[0]), list(map(str, argv)), env or os.environ)
        finally:
            os._exit(127)
    output = bytearray()
    status = None
    supplied = False
    deadline = time.monotonic() + timeout
    try:
        while True:
            if time.monotonic() >= deadline:
                raise subprocess.TimeoutExpired(argv, timeout)
            if select.select([terminal], [], [], min(0.1, deadline - time.monotonic()))[0]:
                try:
                    chunk = os.read(terminal, 65536)
                except OSError as error:
                    if error.errno != errno.EIO:
                        raise
                    chunk = b''
                if not chunk:
                    completed, child_status = os.waitpid(pid, os.WNOHANG)
                    if completed:
                        status = child_status
                        break
                    time.sleep(0.01)
                    continue
                output.extend(chunk)
                if len(output) > 8 << 20:
                    raise RuntimeError('terminal command exceeded output bound')
                tail = bytes(output[-512:]).lower()
                if not supplied and (b'[sudo] password for ' in tail or tail.endswith(b'password: ')):
                    if password is None:
                        raise RuntimeError('administrator prompt needs the test credential on stdin')
                    os.write(terminal, (password + '\n').encode())
                    supplied = True
    finally:
        if status is None:
            try:
                os.killpg(pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            os.waitpid(pid, 0)
        os.close(terminal)
    text = output.decode(errors='replace')
    if password:
        text = text.replace(password, '[redacted]')
    return subprocess.CompletedProcess(argv, os.waitstatus_to_exitcode(status), text, '')
