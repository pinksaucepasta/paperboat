"""Drive a Windows device from the Linux controller: only pb.exe and small PowerShell snippets run there.

The device-smoke checks keep their logic on the controller. Set PB_SMOKE_WINDOWS_PB to the pb.exe path from the
saved `pb install --json` receipt (install_path.py prints it) and pb_path() returns a WindowsPb that
pairing_lifecycle.command_result runs over SSH, so no check needs python3 on the device. VICTUS_PASS (never
printed) and SMOKE_WINDOWS_SSH (default pujan@victus, as smoke.sh uses) pick the login.

Every device call sends a PowerShell script and its JSON payload on stdin behind a one-line -EncodedCommand
bootstrap (the command line stays far below cmd.exe's 8191 characters whatever the payload size). pb runs through
System.Diagnostics.Process with byte-exact stdin/stdout/stderr, an env overlay and a hard timeout that kills the
process tree, and comes back as one JSON envelope, so PowerShell's CLIXML/encoding quirks never touch pb output.
"""
import base64
import contextlib
import functools
import json
import os
import pathlib
import subprocess
import time
import uuid

DEFAULT_TARGET = 'pujan@victus'
MISSING = 3  # exit code of a snippet whose file does not exist

# Reads "<base64 script>\n<base64 JSON payload>" from stdin, then runs the script with $req set.
BOOTSTRAP = ("$ErrorActionPreference='Stop';$ProgressPreference='SilentlyContinue';$a=[Console]::In.ReadToEnd().Split(\"`n\");"
             "$s=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($a[0].Trim()));"
             "$req=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($a[1].Trim()))|ConvertFrom-Json;"
             "Invoke-Expression $s")

RUN = r"""
$ErrorActionPreference='Stop'
$i=New-Object Diagnostics.ProcessStartInfo
$i.FileName=$req.exe;$i.Arguments=$req.args;$i.UseShellExecute=$false
$i.RedirectStandardInput=$true;$i.RedirectStandardOutput=$true;$i.RedirectStandardError=$true
if($req.cwd){$i.WorkingDirectory=$req.cwd}
if($req.env){foreach($e in $req.env.PSObject.Properties){$i.EnvironmentVariables[$e.Name]=[string]$e.Value}}
$p=[Diagnostics.Process]::Start($i)
$om=New-Object IO.MemoryStream;$em=New-Object IO.MemoryStream
$ot=$p.StandardOutput.BaseStream.CopyToAsync($om);$et=$p.StandardError.BaseStream.CopyToAsync($em)
try{$b=[Convert]::FromBase64String($req.input);$p.StandardInput.BaseStream.Write($b,0,$b.Length)}catch{}
try{$p.StandardInput.Close()}catch{}
$t=-not $p.WaitForExit([int]$req.timeout_ms)
if($t){& taskkill.exe /T /F /PID $p.Id *> $null;[void]$p.WaitForExit(10000)}
[void]$ot.Wait(5000);[void]$et.Wait(5000)
@{rc=$(if($t){-1}else{$p.ExitCode});timed_out=$t;out=[Convert]::ToBase64String($om.ToArray());err=[Convert]::ToBase64String($em.ToArray())}|ConvertTo-Json -Compress
"""

READ = ("if(-not(Test-Path -LiteralPath $req.path -PathType Leaf)){exit 3}\n"
        "[Convert]::ToBase64String([IO.File]::ReadAllBytes($req.path))")
WRITE = ("$d=Split-Path -Parent $req.path;if($d){[void](New-Item -ItemType Directory -Force -Path $d)}\n"
         "[IO.File]::WriteAllBytes($req.path,[Convert]::FromBase64String($req.data))")
EXISTS = "if(Test-Path -LiteralPath $req.path){'yes'}else{'no'}"
UNLINK = "Remove-Item -LiteralPath $req.path -Recurse -Force -ErrorAction SilentlyContinue"
MKDIR = "[void](New-Item -ItemType Directory -Force -Path $req.path)"
SHA256 = ("if(-not(Test-Path -LiteralPath $req.path -PathType Leaf)){exit 3}\n"
          "(Get-FileHash -LiteralPath $req.path -Algorithm SHA256).Hash.ToLower()")
SIZE = "if(-not(Test-Path -LiteralPath $req.path -PathType Leaf)){exit 3}\n(Get-Item -LiteralPath $req.path).Length"
ENV = ("if($req.name -eq 'STATE'){if($env:XDG_STATE_HOME){$env:XDG_STATE_HOME}"
       "else{[Environment]::GetFolderPath('LocalApplicationData')}}"
       "else{[Environment]::GetEnvironmentVariable($req.name)}")
SERVICES = ("@(Get-Service -Name $req.pattern -ErrorAction SilentlyContinue|"
            "Select-Object Name,@{n='Status';e={[string]$_.Status}},@{n='StartType';e={[string]$_.StartType}})|"
            "ConvertTo-Json -Compress")
# The child never inherits the SSH session's pipes (Windows OpenSSH keeps a session open while a child holds them):
# all three standard handles go to files in $req.dir. $IsWindows is undefined (so true) in Windows PowerShell 5.1;
# -WindowStyle does not exist on pwsh for Linux.
START = ("$z=Join-Path $req.dir 'start.in';[IO.File]::WriteAllBytes($z,[byte[]]@())\n"
         "$o=@{FilePath=$req.exe;PassThru=$true;RedirectStandardInput=$z;"
         "RedirectStandardOutput=(Join-Path $req.dir 'start.out');RedirectStandardError=(Join-Path $req.dir 'start.err')}\n"
         "if($IsWindows -ne $false){$o.WindowStyle='Hidden'}\n"
         "if($req.args){$o.ArgumentList=$req.args}\n"
         "(Start-Process @o).Id")
STOP = ("& taskkill.exe /T /F /PID $req.pid *> $null\n"
        "if(Get-Process -Id $req.pid -ErrorAction SilentlyContinue){exit 1}")
PROCESSES = ("@(Get-CimInstance Win32_Process|Where-Object{$_.CommandLine -match $req.pattern}|"
             "Select-Object ProcessId,Name)|ConvertTo-Json -Compress")
TASKS = ("@(Get-ScheduledTask -TaskPath $req.path -ErrorAction SilentlyContinue|"
         "Select-Object TaskName,@{n='State';e={[string]$_.State}})|ConvertTo-Json -Compress")
SERVICE_PATHS = ("@(Get-CimInstance Win32_Service|Where-Object{$_.Name -like $req.pattern}|"
                 "Select-Object Name,PathName)|ConvertTo-Json -Compress")
ALIVE = "if(Get-Process -Id $req.pid -ErrorAction SilentlyContinue){'yes'}else{'no'}"

# Runs inside the detached wrapper: pb with output in files and the exit code in `rc` (Start-Process writes raw bytes).
SPAWN = ("$o=@{FilePath=$req.exe;RedirectStandardInput=$req.inp;RedirectStandardOutput=$req.out;"
         "RedirectStandardError=$req.err;PassThru=$true;Wait=$true}\n"
         "if($IsWindows -ne $false){$o.WindowStyle='Hidden'}\n"
         "if($req.args){$o.ArgumentList=$req.args}\n"
         "$p=Start-Process @o\n"
         "Set-Content -LiteralPath $req.rc -Value $p.ExitCode -Encoding ascii")

# A one-connection-at-a-time HTTP origin on the device's loopback; its port goes to $req.port_file.
ORIGIN = r"""
$l=New-Object Net.Sockets.TcpListener([Net.IPAddress]::Loopback,0);$l.Start()
[IO.File]::WriteAllText($req.port_file,[string]$l.LocalEndpoint.Port)
$b=[Text.Encoding]::UTF8.GetBytes($req.body)
$h=[Text.Encoding]::ASCII.GetBytes("HTTP/1.1 200 OK`r`nContent-Type: text/html`r`nContent-Length: $($b.Length)`r`nConnection: close`r`n`r`n")
while($true){$c=$l.AcceptTcpClient();try{$c.ReceiveTimeout=2000;$s=$c.GetStream();$r=New-Object byte[] 8192
[void]$s.Read($r,0,8192);$s.Write($h,0,$h.Length);$s.Write($b,0,$b.Length);$s.Flush()}catch{}finally{$c.Close()}}
"""


def _b64(value):
    return base64.b64encode(value).decode('ascii')


def ps_quote(value):
    return "'" + str(value).replace("'", "''") + "'"


def _text(value):
    return value.decode(errors='replace') if isinstance(value, (bytes, bytearray)) else value or ''


class DeviceError(RuntimeError):
    """A device snippet failed; the message names the step."""


class WindowsDevice:
    path_flavor = pathlib.PureWindowsPath  # tests that run the snippets in a local pwsh use PurePosixPath

    def __init__(self, target=None, pb_exe=None, password=None):
        self.target = target or os.environ.get('SMOKE_WINDOWS_SSH', DEFAULT_TARGET)
        self.pb_exe = pb_exe
        self.password = os.environ.get('VICTUS_PASS') if password is None else password
        self._environment = {}

    def ssh_command(self):
        ssh = ['ssh', '-o', 'ConnectTimeout=15', '-o', 'ServerAliveInterval=30', '-o', 'StrictHostKeyChecking=accept-new',
               self.target, 'powershell -NoProfile -NonInteractive -InputFormat Text -OutputFormat Text '
               '-EncodedCommand ' + _b64(BOOTSTRAP.encode('utf-16-le'))]
        if self.password:
            return ['sshpass', '-e'] + ssh
        return ['ssh', '-o', 'BatchMode=yes'] + ssh[1:]

    def snippet(self, step, script, timeout=60, **payload):
        """Run script on the device with $req = payload; return (rc, stdout bytes, stderr bytes)."""
        stdin = _b64(script.encode()) + '\n' + _b64(json.dumps(payload).encode()) + '\n'
        environment = dict(os.environ, SSHPASS=self.password) if self.password else None
        try:
            done = subprocess.run(self.ssh_command(), input=stdin.encode(), capture_output=True, timeout=timeout,
                                  env=environment)
        except subprocess.TimeoutExpired as error:
            raise DeviceError('%s: the device did not answer within %ss; check that victus is reachable' %
                              (step, timeout)) from error
        return done.returncode, done.stdout, done.stderr

    def ps(self, step, script, timeout=60, ok=(0,), **payload):
        rc, out, err = self.snippet(step, script, timeout, **payload)
        if rc not in ok:
            raise DeviceError('%s failed on the device (exit %s): %s' % (step, rc, _text(err).strip()[-300:]))
        return rc, _text(out).strip()

    def run(self, argv, input=None, env=None, timeout=90, cwd=None, text=True):
        """Run argv[0] on the device like subprocess.run(..., capture_output=True, text=text)."""
        data = input.encode() if isinstance(input, str) else (input or b'')
        rc, out, err = self.snippet('run ' + pathlib.PureWindowsPath(argv[0]).name, RUN, timeout + 30,
                                    exe=argv[0], args=subprocess.list2cmdline(list(argv[1:])), env=env or {},
                                    cwd=cwd or '', input=_b64(data), timeout_ms=int(timeout * 1000))
        if rc:
            raise DeviceError('run %s failed on the device (exit %s): %s' %
                              (pathlib.PureWindowsPath(argv[0]).name, rc, _text(err).strip()[-300:]))
        try:
            envelope = json.loads(_text(out).strip().splitlines()[-1])
            stdout, stderr = base64.b64decode(envelope['out']), base64.b64decode(envelope['err'])
        except (ValueError, IndexError, KeyError):
            raise DeviceError('run %s returned no result envelope' % argv[0]) from None
        if envelope['timed_out']:
            raise subprocess.TimeoutExpired(argv, timeout, output=stdout, stderr=stderr)
        if not text:
            return subprocess.CompletedProcess(argv, envelope['rc'], stdout, stderr)
        return subprocess.CompletedProcess(argv, envelope['rc'], _text(stdout), _text(stderr))

    def read_bytes(self, path):
        rc, out = self.ps('read %s' % path, READ, ok=(0, MISSING), path=str(path))
        if rc == MISSING:
            raise FileNotFoundError(path)
        return base64.b64decode(out)

    def write_bytes(self, path, data):
        self.ps('write %s' % path, WRITE, path=str(path), data=_b64(data))

    def exists(self, path):
        return self.ps('exists %s' % path, EXISTS, path=str(path))[1] == 'yes'

    def unlink(self, path):
        self.ps('remove %s' % path, UNLINK, path=str(path))

    def mkdir(self, path):
        self.ps('mkdir %s' % path, MKDIR, path=str(path))

    def sha256(self, path):
        rc, out = self.ps('hash %s' % path, SHA256, ok=(0, MISSING), path=str(path))
        if rc == MISSING:
            raise FileNotFoundError(path)
        return out

    def size(self, path):
        rc, out = self.ps('size %s' % path, SIZE, ok=(0, MISSING), path=str(path))
        if rc == MISSING:
            raise FileNotFoundError(path)
        return int(out)

    def env(self, name):
        if name not in self._environment:
            self._environment[name] = self.ps('environment ' + name, ENV, name=name)[1]
        return self._environment[name]

    def services(self, pattern='Paperboat*'):
        return self._list('services ' + pattern, SERVICES, pattern=pattern)

    def _list(self, step, script, **payload):
        out = self.ps(step, script, **payload)[1]
        value = json.loads(out) if out else []
        return [value] if isinstance(value, dict) else value

    def processes(self, pattern):
        """Processes whose command line matches the regex (pgrep -f). The snippet's own command line is only a
        base64 bootstrap, so it never matches."""
        return self._list('processes ' + pattern, PROCESSES, pattern=pattern)

    def scheduled_tasks(self, path):
        return self._list('tasks ' + path, TASKS, path=path)

    def service_paths(self, pattern):
        """Name and command line (PathName) of the services whose name matches the wildcard."""
        return self._list('service paths ' + pattern, SERVICE_PATHS, pattern=pattern)

    def start(self, argv, directory):
        """Start argv[0] hidden and detached, its standard handles in directory/start.*; return its pid."""
        return int(self.ps('start ' + argv[0], START, exe=argv[0], args=subprocess.list2cmdline(list(argv[1:])),
                           dir=str(directory))[1])

    def stop(self, pid):
        self.ps('stop %s' % pid, STOP, pid=int(pid))

    def alive(self, pid):
        return self.ps('alive %s' % pid, ALIVE, pid=int(pid))[1] == 'yes'

    def spawn(self, argv, directory):
        """Start argv detached with output in directory/{out,err} and its exit code in directory/rc."""
        directory = RemotePath(self, directory)
        args = subprocess.list2cmdline(list(argv[1:]))
        wrapper = _b64(('$req=@{exe=%s;args=%s;inp=%s;out=%s;err=%s;rc=%s};' % tuple(map(ps_quote, (
            argv[0], args, directory / 'start.in', directory / 'out', directory / 'err', directory / 'rc'))) + SPAWN
        ).encode('utf-16-le'))
        pid = self.start(['powershell.exe', '-NoProfile', '-NonInteractive', '-EncodedCommand', wrapper], directory)
        return RemoteProcess(self, pid, directory)

    @contextlib.contextmanager
    def origin(self, body):
        """An HTTP origin on the device's loopback that answers every request with body; yields its port."""
        with self.temp_dir('pb-origin') as directory:
            port_file = directory / 'port'
            script = ('$req=@{port_file=%s;body=%s};' % (ps_quote(port_file), ps_quote(body)) + ORIGIN)
            pid = self.start(['powershell.exe', '-NoProfile', '-NonInteractive', '-EncodedCommand',
                              _b64(script.encode('utf-16-le'))], directory)
            try:
                deadline = time.monotonic() + 30
                while not port_file.exists():
                    if time.monotonic() > deadline:
                        raise DeviceError('origin did not start listening within 30s on the device')
                    time.sleep(0.5)
                yield int(port_file.read_text().strip())
            finally:
                self.stop(pid)

    def home(self):
        return RemotePath(self, self.env('USERPROFILE'))

    def runtime_dir(self):
        """Where `pb pair` keeps machine-registration.json: userpaths.State("Paperboat/runtime")."""
        return RemotePath(self, self.env('STATE')) / 'Paperboat' / 'runtime'

    @contextlib.contextmanager
    def temp_dir(self, prefix='pb-smoke'):
        path = RemotePath(self, self.env('TEMP')) / ('%s-%s' % (prefix, uuid.uuid4().hex[:12]))
        path.mkdir()
        try:
            yield path
        finally:
            path.unlink()


class RemoteProcess:
    """A process started by WindowsDevice.spawn: read its output so far, wait for its exit code, kill its tree."""

    def __init__(self, device, pid, directory):
        self.device, self.pid, self.directory = device, pid, directory

    def _read(self, name):
        try:
            return (self.directory / name).read_bytes()
        except FileNotFoundError:
            return b''

    def stdout(self):
        return self._read('out')

    def stderr(self):
        return self._read('err')

    def returncode(self):
        value = self._read('rc').decode().strip()
        return int(value) if value else None

    def wait(self, timeout, interval=2):
        deadline = time.monotonic() + timeout
        while self.returncode() is None:
            if time.monotonic() > deadline:
                raise subprocess.TimeoutExpired(str(self.directory), timeout, output=self.stdout(), stderr=self.stderr())
            time.sleep(interval)
        return self.returncode()

    def kill(self):
        self.device.stop(self.pid)


class RemotePath:
    """The part of pathlib.Path the checks use, executed on the device."""

    def __init__(self, device, path):
        self.device, self.path = device, device.path_flavor(str(path))

    def __truediv__(self, name):
        return RemotePath(self.device, self.path / name)

    def __str__(self):
        return str(self.path)

    def __eq__(self, other):
        return isinstance(other, RemotePath) and self.path == other.path

    @property
    def name(self):
        return self.path.name

    @property
    def parent(self):
        return RemotePath(self.device, self.path.parent)

    def read_bytes(self):
        return self.device.read_bytes(self)

    def read_text(self):
        return self.read_bytes().decode()

    def write_bytes(self, data):
        self.device.write_bytes(self, data)

    def write_text(self, value):
        self.write_bytes(value.encode())

    def exists(self):
        return self.device.exists(self)

    def mkdir(self, parents=True, exist_ok=True):
        self.device.mkdir(self)

    def unlink(self, missing_ok=True):
        self.device.unlink(self)

    def chmod(self, mode):
        pass  # files under the user's profile are private to that user by their inherited ACL

    def sha256(self):
        return self.device.sha256(self)

    def size(self):
        return self.device.size(self)


class WindowsPb:
    """The pb.exe on the device; str() is its path, run_remote() runs it there."""

    def __init__(self, device):
        self.device = device

    def __str__(self):
        return self.device.pb_exe

    def run_remote(self, arguments, input=None, env=None, timeout=180, text=True):
        return self.device.run([self.device.pb_exe, *arguments], input=input, env=env, timeout=timeout, text=text)


def device():
    """The Windows device when PB_SMOKE_WINDOWS_PB is set, else None (the check runs on a Unix device)."""
    exe = os.environ.get('PB_SMOKE_WINDOWS_PB')
    if not exe:
        return None
    path = pathlib.PureWindowsPath(exe)
    if not path.is_absolute() or path.name.lower() != 'pb.exe':
        raise ValueError('PB_SMOKE_WINDOWS_PB must be an absolute Windows pb.exe path')
    return _device(exe)


@functools.lru_cache(maxsize=None)
def _device(exe):  # one object per exe so its environment lookups are cached
    return WindowsDevice(pb_exe=exe)


def true_argv():
    """The command that exits 0 on this device itself, for `pb exec <this machine> -- ...`."""
    return ['cmd', '/c', 'exit', '0'] if device() else ['true']


def pb_path():
    """Replaces Path.home() / '.local/bin/pb' in the checks."""
    target = device()
    return WindowsPb(target) if target else pathlib.Path.home() / '.local/bin/pb'


def runtime_dir(default):
    target = device()
    return target.runtime_dir() if target else default
