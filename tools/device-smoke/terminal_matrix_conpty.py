"""terminal_matrix N6 driver, run ON the Windows client by terminal_matrix.py: `pb connect` in a local ConPTY, as a user's terminal runs it.

Through the controller's pty -> `ssh -tt` -> ConPTY hop the 0x03 is lost or delayed under a flood (plain ssh.exe fails there 19/20; pb in a local
ConPTY 20/20), so the Windows client's Ctrl-C row types here instead. Usage: python terminal_matrix_conpty.py PB TARGET SESSION BASELINE
BASELINE is the number of `yes` already running on the target. One JSON object per line, flushed: {"flood": n}, {"sent": target-clock time of
the Ctrl-C}, {"quiet": seconds the output took to go quiet, or null}, {"answered": the shell printed a marker computed by the command}, or {"error": why}."""
import ctypes
import json
import subprocess
import sys
import threading
import time
import uuid
from ctypes import wintypes

k32 = ctypes.windll.kernel32

PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE = 0x00020016
EXTENDED_STARTUPINFO_PRESENT = 0x00080000
CREATE_UNICODE_ENVIRONMENT = 0x00000400
STARTF_USESTDHANDLES = 0x00000100
HANDLE_FLAG_INHERIT = 0x00000001


class COORD(ctypes.Structure):
    _fields_ = [('X', wintypes.SHORT), ('Y', wintypes.SHORT)]


class SECURITY_ATTRIBUTES(ctypes.Structure):
    _fields_ = [('nLength', wintypes.DWORD), ('lpSecurityDescriptor', wintypes.LPVOID),
                ('bInheritHandle', wintypes.BOOL)]


class STARTUPINFO(ctypes.Structure):
    _fields_ = [('cb', wintypes.DWORD), ('lpReserved', wintypes.LPWSTR), ('lpDesktop', wintypes.LPWSTR),
                ('lpTitle', wintypes.LPWSTR), ('dwX', wintypes.DWORD), ('dwY', wintypes.DWORD),
                ('dwXSize', wintypes.DWORD), ('dwYSize', wintypes.DWORD),
                ('dwXCountChars', wintypes.DWORD), ('dwYCountChars', wintypes.DWORD),
                ('dwFillAttribute', wintypes.DWORD), ('dwFlags', wintypes.DWORD),
                ('wShowWindow', wintypes.WORD), ('cbReserved2', wintypes.WORD),
                ('lpReserved2', wintypes.LPBYTE), ('hStdInput', wintypes.HANDLE),
                ('hStdOutput', wintypes.HANDLE), ('hStdError', wintypes.HANDLE)]


class STARTUPINFOEX(ctypes.Structure):
    _fields_ = [('StartupInfo', STARTUPINFO), ('lpAttributeList', wintypes.LPVOID)]


class PROCESS_INFORMATION(ctypes.Structure):
    _fields_ = [('hProcess', wintypes.HANDLE), ('hThread', wintypes.HANDLE),
                ('dwProcessId', wintypes.DWORD), ('dwThreadId', wintypes.DWORD)]


def _winerr(stage):
    code = k32.GetLastError()
    buf = ctypes.create_unicode_buffer(512)
    k32.FormatMessageW(0x00001000, None, code, 0, buf, 512, None)
    raise RuntimeError('%s failed: win32=%d %s' % (stage, code, buf.value.strip()))


class ConPTYSession:
    def __init__(self, cmdline, rows=24, cols=80):
        sa = SECURITY_ATTRIBUTES(ctypes.sizeof(SECURITY_ATTRIBUTES), None, True)
        self.hInRead = wintypes.HANDLE()
        self.hInWrite = wintypes.HANDLE()
        self.hOutRead = wintypes.HANDLE()
        self.hOutWrite = wintypes.HANDLE()
        if not k32.CreatePipe(ctypes.byref(self.hInRead), ctypes.byref(self.hInWrite), ctypes.byref(sa), 65536):
            _winerr('CreatePipe(in)')
        if not k32.CreatePipe(ctypes.byref(self.hOutRead), ctypes.byref(self.hOutWrite), ctypes.byref(sa), 65536):
            _winerr('CreatePipe(out)')
        self.hpc = wintypes.HANDLE()
        if k32.CreatePseudoConsole(COORD(cols, rows), self.hInRead, self.hOutWrite, 0, ctypes.byref(self.hpc)) != 0:
            _winerr('CreatePseudoConsole')
        size = ctypes.c_size_t(0)
        k32.InitializeProcThreadAttributeList(None, 1, 0, ctypes.byref(size))
        attr = (ctypes.c_byte * size.value)()
        pattr = ctypes.cast(attr, wintypes.LPVOID)
        if not k32.InitializeProcThreadAttributeList(pattr, 1, 0, ctypes.byref(size)):
            _winerr('InitializeProcThreadAttributeList')
        # PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE takes the HPCON handle VALUE
        # reinterpreted as a pointer (repo pty_windows.go); &hpc attaches a dead
        # pointer and the child never exchanges I/O with the pty.
        hpc_val = ctypes.c_void_p(self.hpc.value)
        if not k32.UpdateProcThreadAttribute(pattr, 0, PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
                                             hpc_val, ctypes.sizeof(wintypes.HANDLE), None, None):
            _winerr('UpdateProcThreadAttribute')
        si = STARTUPINFOEX()
        si.StartupInfo.cb = ctypes.sizeof(STARTUPINFOEX)
        si.StartupInfo.dwFlags = STARTF_USESTDHANDLES
        si.lpAttributeList = pattr
        pi = PROCESS_INFORMATION()
        buf = ctypes.create_unicode_buffer(cmdline)
        ok = k32.CreateProcessW(None, buf, None, None, False,
                                EXTENDED_STARTUPINFO_PRESENT | CREATE_UNICODE_ENVIRONMENT,
                                None, None, ctypes.byref(si.StartupInfo), ctypes.byref(pi))
        k32.DeleteProcThreadAttributeList(pattr)
        if not ok:
            _winerr('CreateProcess')
        k32.CloseHandle(pi.hThread)
        self.proc = pi.hProcess
        k32.CloseHandle(self.hInRead)
        k32.CloseHandle(self.hOutWrite)
        self.buf = bytearray()
        self.lock = threading.Lock()
        self.last_data = time.monotonic()
        self.eof = threading.Event()
        self.pump = threading.Thread(target=self._pump, daemon=True)
        self.pump.start()

    def _pump(self):
        chunk = ctypes.create_string_buffer(65536)
        got = wintypes.DWORD(0)
        while True:
            ok = k32.ReadFile(self.hOutRead, chunk, 65536, ctypes.byref(got), None)
            if not ok or got.value == 0:
                self.eof.set()
                return
            with self.lock:
                self.buf.extend(chunk.raw[:got.value])
                if len(self.buf) > (1 << 20):
                    del self.buf[:-(1 << 19)]
            self.last_data = time.monotonic()

    def write(self, data):
        if isinstance(data, str):
            data = data.encode()
        off = 0
        wrote = wintypes.DWORD(0)
        while off < len(data):
            if not k32.WriteFile(self.hInWrite, data[off:off + 65536], min(65536, len(data) - off),
                                 ctypes.byref(wrote), None):
                _winerr('WriteFile')
            off += wrote.value

    def scan(self, token, timeout):
        token = token.encode() if isinstance(token, str) else token
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            with self.lock:
                if token in self.buf:
                    return True
            if self.eof.is_set():
                return False
            time.sleep(0.1)
        return False

    def quiet_after(self, quiet, timeout):
        t0 = time.monotonic()
        while time.monotonic() - t0 < timeout:
            if time.monotonic() - self.last_data >= quiet:
                return time.monotonic() - t0
            time.sleep(0.1)
        return None

    def close(self):
        try:
            k32.TerminateProcess(self.proc, 1)
        except Exception:
            pass
        try:
            k32.CloseHandle(self.proc)
        except Exception:
            pass
        try:
            k32.ClosePseudoConsole(self.hpc)
        except Exception:
            pass


PASTE = ('x' * 1023 + '\n') * 256
HOLD = 3.0  # seconds of flood before the paste and Ctrl-C


def emit(**fields):
    print(json.dumps(fields), flush=True)


def target_time(pb, target):
    """The target's clock now, and the round trip it took."""
    t0 = time.time()
    out = subprocess.run([pb, 'exec', target, '--', 'python3', '-c', 'import time;print(time.time())'], capture_output=True, text=True, timeout=60).stdout
    t1 = time.time()
    return float(out.strip().splitlines()[-1]), t1 - t0, (t0 + t1) / 2


def clock_offset(pb, target):
    """Target clock minus this machine's, from the quickest of three round trips (the error is half of it)."""
    best = min((target_time(pb, target) for _ in range(3)), key=lambda sample: sample[1])
    return best[0] - best[2]


def yes_count(pb, target):
    out = subprocess.run([pb, 'exec', target, '--', 'sh', '-c', 'pgrep -x -c yes || true'], capture_output=True, text=True, timeout=60).stdout
    return int(out.strip().splitlines()[-1] or 0)


def main(pb, target, session, baseline):
    s = ConPTYSession('"%s" connect %s --session %s --status-bar off' % (pb, target, session))
    try:
        # The marker is computed by the shell: the typed line holds "%s", so only the command's output holds the marker.
        ready = uuid.uuid4().hex
        s.write("printf 'PBRDY%%s\\n' %s\r" % ready)
        if not s.scan('PBRDY' + ready, 60):
            return emit(error='no shell answer in 60 s')
        offset = clock_offset(pb, target)
        s.write('yes\r')
        t0 = time.monotonic()
        while yes_count(pb, target) <= baseline:
            if time.monotonic() - t0 > 30:
                return emit(error='yes did not start on the target in 30 s')
        emit(flood=yes_count(pb, target))
        time.sleep(max(0, HOLD - (time.monotonic() - t0)))
        s.write(PASTE)
        s.write('\x03')
        emit(sent=time.time() + offset)
        emit(quiet=s.quiet_after(1.0, 60))
        answer = uuid.uuid4().hex
        s.write("printf 'PBANS%%s\\n' %s\r" % answer)
        emit(answered=s.scan('PBANS' + answer, 15))
    finally:
        s.close()


if __name__ == '__main__':
    try:
        main(sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4]))
    except Exception as error:  # the controller reads this line; a traceback it cannot parse would hide the cause
        emit(error='%s: %s' % (type(error).__name__, str(error)[:200]))
