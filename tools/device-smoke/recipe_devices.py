"""The devices of the recipe checks (recipe_checks.py): a Linux container, a real Linux or macOS machine over SSH, a
Windows machine over SSH. Each runs pb, runs the installer one-liner with its secret on stdin (never in argv) and says
whether Paperboat is already on it. Everything printed back goes through redact()."""
import json
import re
import shlex
import subprocess
import time

import ct_env

PB_RECIPE_RE = re.compile(r'pbr_[a-z2-7]{10}_[a-z2-7]{52}')
SECRET_RES = (PB_RECIPE_RE, re.compile(r'pbat_[A-Za-z0-9_-]{16,}'), re.compile(r'(?i)(code=)[A-Z0-9-]{6,}'))
SECRETS = set()  # run-time secrets with no recognisable shape: ENV passwords, enrollment tokens, recipe secrets
# The one-liners `pb recipe create` prints. They come from the server, so only exactly these shapes are ever run.
POSIX_LINE = re.compile(r'curl -fsSL (https://[A-Za-z0-9.-]+(?::\d+)?(?:/[A-Za-z0-9._/-]*)?) \| sh')
WINDOWS_LINE = re.compile(r'irm (https://[A-Za-z0-9.-]+(?::\d+)?(?:/[A-Za-z0-9._/-]*)?) \| iex')
VARIABLES = ('PB_RECIPE', 'PB_TOKEN')


def redact(text):
    for secret in sorted(SECRETS, key=len, reverse=True):
        text = text.replace(secret, '<redacted>')
    for pattern in SECRET_RES:
        text = pattern.sub(lambda m: (m.group(1) if pattern.groups else m.group(0)[:4]) + '<redacted>', text)
    return text


def installer_url(line, windows=False):
    """The URL of a printed one-liner; anything else is refused rather than executed."""
    match = (WINDOWS_LINE if windows else POSIX_LINE).fullmatch(line.strip())
    if not match:
        raise AssertionError('the printed %s install line is not the one-liner: %r' % ('Windows' if windows else 'Linux/macOS', redact(line)[:200]))
    return match.group(1)


class Result:
    def __init__(self, code, out, err=''):
        self.code, self.out, self.err = code, out, err

    @property
    def text(self):
        return self.out + self.err

    def tail(self, size=300):
        return ' '.join(redact(self.text).split())[-size:]


def spawn(argv, stdin=None, timeout=300):
    try:
        done = subprocess.run(argv, input=stdin if stdin is not None else '', capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired as error:
        partial = error.stdout.decode(errors='replace') if isinstance(error.stdout, bytes) else error.stdout or ''
        return Result(124, partial, '\ntimed out after %d s' % timeout)
    return Result(done.returncode, done.stdout, done.stderr)


def parse_record(done, what):
    """(modification stamp, parsed recipe-apply.json) from "stamp\n{json}"."""
    stamp, _, body = done.out.strip().partition('\n')
    try:
        return int(stamp.strip()), json.loads(body)
    except ValueError:
        raise AssertionError('no recipe apply record on the %s machine: %s' % (what, done.tail())) from None


# pb as the installer leaves it on Linux and macOS
PB_FIND = ('P=; for c in "$HOME/.local/bin/pb" "/Library/PrivilegedHelperTools/Paperboat/users/u$(id -u)/bin/pb"; do '
           '[ -x "$c" ] && { P=$c; break; }; done; [ -n "$P" ] || { echo "pb is not installed" >&2; exit 127; }; ')
# anything of an install or a sign-in: a recipe check only ever starts on a machine with none of it
POSIX_PRESENT = ('for p in "$HOME/.local/bin/pb" "/usr/local/libexec/paperboat/users/u$(id -u)" '
                 '"/Library/PrivilegedHelperTools/Paperboat/users/u$(id -u)" "$HOME/.config/paperboat/config.json" '
                 '"$HOME/Library/Application Support/paperboat/config.json"; do [ -e "$p" ] && echo "$p"; done; '
                 'command -v pb | grep /; echo END')
# what the daemon has applied of the machine's recipe, beside pb's config file
POSIX_APPLY_RECORD = ('for f in "$HOME/.config/paperboat/recipe-apply.json" "$HOME/Library/Application Support/paperboat/recipe-apply.json"; do '
                      '[ -f "$f" ] && { stat -c %Y "$f" 2>/dev/null || stat -f %m "$f"; cat "$f"; exit 0; }; done; echo "no recipe-apply.json" >&2; exit 3')
# the line `pb env hook` (installed by pairing) adds to a shell startup file
HOOK_MARK = '# paperboat-env-hook'
HOOK_FILES = ' '.join('"$HOME/%s"' % name for name in ('.bashrc', '.bash_profile', '.bash_login', '.profile', '.zshrc', '.zshenv', '.zprofile', '.config/fish/config.fish'))


class Posix:
    """kind 'container' (a docker container on host), 'linux' or 'macos' (an SSH host used as itself)."""

    def __init__(self, kind, host, name=None, user=None):
        self.kind, self.host, self.name, self.user = kind, host, name, user
        self.os = 'macos' if kind == 'macos' else 'linux'

    def argv(self, script):
        run = 'bash -lc ' + shlex.quote(script)
        if self.kind == 'container':
            who = '-u %s -e HOME=/home/%s -w /home/%s' % (self.user, self.user, self.user) if self.user else ct_env.CT_EXEC + ' -w ' + ct_env.CT_HOME
            run = '%s exec -i %s %s %s' % (ct_env.CT_DOCKER, who, self.name, run)
        return ['ssh', '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=accept-new', '-o', 'ConnectTimeout=20',
                '-o', 'ServerAliveInterval=30', self.host, run]

    def sh(self, script, stdin=None, timeout=300):
        for attempt in range(3):
            done = spawn(self.argv(script), stdin, timeout)
            # ssh gave up before the SSH handshake: nothing ran on the machine, so the same command is safe to send again
            if attempt == 2 or done.code != 255 or 'during banner exchange' not in done.err:
                return done
            time.sleep(5)

    def pb(self, *args, stdin=None, timeout=180):
        return self.sh(PB_FIND + '"$P" ' + ' '.join(shlex.quote(a) for a in args), stdin, timeout)

    def present(self):
        """What of Paperboat is already here (empty: a clean machine)."""
        done = self.sh(POSIX_PRESENT, timeout=90)
        lines = done.out.split()
        if done.code or not lines or lines[-1] != 'END':
            raise AssertionError('cannot look for Paperboat on the %s machine: %s' % (self.kind, done.tail()))
        return lines[:-1]

    def hooks(self):
        """Shell startup files that carry Paperboat's ENV hook line."""
        done = self.sh('grep -l %s %s 2>/dev/null; echo END' % (shlex.quote(HOOK_MARK), HOOK_FILES), timeout=90)
        lines = done.out.split('\n')
        lines = [line.strip() for line in lines if line.strip()]
        if done.code or not lines or lines[-1] != 'END':
            raise AssertionError('cannot read the shell startup files: ' + done.tail())
        return lines[:-1]

    def apply_record(self):
        return parse_record(self.sh(POSIX_APPLY_RECORD, timeout=90), self.kind)

    def hook_lines(self):
        """{shell startup file: how many ENV hook lines it carries}."""
        counts = {}
        for path in self.hooks():
            done = self.sh('grep -c %s %s' % (shlex.quote(HOOK_MARK), shlex.quote(path)), timeout=90)
            counts[path] = int(done.out.strip() or 0)
        return counts

    def start_app(self, port, marker):
        """A local web server answering marker on 127.0.0.1:port; returns its PID (stop_app ends exactly it)."""
        done = self.sh('d="$HOME/rcp-work/app%d"; mkdir -p "$d" && printf %%s %s > "$d/index.html" || exit 1; '
                       'nohup python3 -m http.server %d --bind 127.0.0.1 --directory "$d" >/dev/null 2>&1 </dev/null & echo $!; '
                       'sleep 2; curl -fsS --max-time 10 http://127.0.0.1:%d/' % (port, shlex.quote(marker), port, port), timeout=90)
        lines = done.out.split()
        if done.code or not lines or not lines[0].isdigit() or marker not in done.out:
            raise AssertionError('the local app on port %d did not start: %s' % (port, done.tail()))
        return int(lines[0])

    def stop_app(self, pid):
        self.sh('kill %d 2>/dev/null; true' % pid, timeout=60)

    def drop_hooks(self):
        """Runner hygiene after the check judged the uninstall: take any hook line out."""
        self.sh('for f in %s; do [ -f "$f" ] && grep -q %s "$f" && { grep -v %s "$f" > "$f.rcp" ; cat "$f.rcp" > "$f"; rm -f "$f.rcp"; }; done; true'
                % (HOOK_FILES, shlex.quote(HOOK_MARK), shlex.quote(HOOK_MARK)), timeout=90)

    def one_liner_script(self, url, variable, name, version=None, extra=''):
        if variable not in VARIABLES:
            raise ValueError('unknown installer variable ' + variable)
        exports = 'PB_NAME=' + shlex.quote(name) + (' PB_VERSION=' + shlex.quote(version) if version else '')
        return ('IFS= read -r v; export %s="$v" %s; set -o pipefail; curl -fsSL %s | sh %s 2>&1' % (variable, exports, shlex.quote(url), extra)).rstrip()

    def one_liner(self, url, variable, value, name, version=None, timeout=600):
        """The printed one-liner with the variable's value read from stdin. The installer's stdin is the curl pipe: it cannot prompt."""
        return self.sh(self.one_liner_script(url, variable, name, version), stdin=value + '\n', timeout=timeout)

    def hostname(self):
        done = self.sh('hostname', timeout=60)
        if done.code or not done.out.strip():
            raise AssertionError('hostname failed: ' + done.tail())
        return done.out.strip()

    def print_env_args(self, key):
        return ['sh', '-c', 'printf %s "$' + key + '"']

    def write(self, filename, text):
        """filename in a work directory of the device; returns the path pb takes."""
        done = self.sh('mkdir -p "$HOME/rcp-work" && cat > "$HOME/rcp-work/%s" && echo "$HOME/rcp-work/%s"' % (shlex.quote(filename), shlex.quote(filename)),
                       stdin=text, timeout=60)
        if done.code:
            raise AssertionError('write %s: %s' % (filename, done.tail()))
        return done.out.strip().splitlines()[-1]

    def uninstall(self):
        done = self.sh(PB_FIND + '"$P" uninstall --confirmation "UNINSTALL PAPERBOAT" --hostname "$(hostname)" 2>&1; rc=$?; rm -rf "$HOME/rcp-work"; exit $rc', timeout=420)
        if done.code and 'pb is not installed' not in done.text:
            raise AssertionError('pb uninstall: ' + done.tail())


# The installer runs in a child PowerShell, as pasting the line does; its exit code and every stream come back.
WINDOWS_ONE_LINER = r'''
$ProgressPreference = 'SilentlyContinue'
[Environment]::SetEnvironmentVariable($req.variable, $req.value, 'Process')
$env:PB_NAME = $req.name
if ($req.version) { $env:PB_VERSION = $req.version }
$ErrorActionPreference = 'Continue'
& powershell -NoProfile -NonInteractive -OutputFormat Text -Command ('irm ' + $req.url + ' | iex') 2>&1 | ForEach-Object { "$_" }
exit $LASTEXITCODE
'''
WINDOWS_PRESENT = r'''
foreach ($p in 'C:\Program Files\Paperboat\users', (Join-Path $env:APPDATA 'paperboat\config.json'), (Join-Path $env:USERPROFILE '.config\paperboat\config.json')) {
  if (Test-Path -LiteralPath $p) {
    if (-not (Get-Item -LiteralPath $p).PSIsContainer) { $p }
    else { try { Get-ChildItem -LiteralPath $p -Recurse -Filter pb.exe -ErrorAction Stop | ForEach-Object FullName } catch { $p } }
  }
}
Get-Service -Name 'Paperboat*' -ErrorAction SilentlyContinue | ForEach-Object Name
'END'
'''
WINDOWS_APPLY_RECORD = r'''
foreach ($d in (Join-Path $env:APPDATA 'paperboat'), (Join-Path $env:USERPROFILE '.config\paperboat')) {
  $p = Join-Path $d 'recipe-apply.json'
  if (Test-Path -LiteralPath $p) { (Get-Item -LiteralPath $p).LastWriteTimeUtc.Ticks; Get-Content -Raw -LiteralPath $p; exit 0 }
}
[Console]::Error.WriteLine('no recipe-apply.json'); exit 3
'''
# a web server outside the SSH session (WMI starts it, so closing the connection does not end it)
WINDOWS_START_APP = r'''
$d = Join-Path $env:TEMP ('rcp-app' + $req.port)
New-Item -ItemType Directory -Force -Path $d | Out-Null
[IO.File]::WriteAllText((Join-Path $d 'index.html'), [string]$req.marker)
$python = (Get-Command python -ErrorAction Stop).Source
$line = '"' + $python + '" -m http.server ' + $req.port + ' --bind 127.0.0.1 --directory "' + $d + '"'
$made = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{ CommandLine = $line }
if ($made.ReturnValue -ne 0) { throw ('could not start the web server: ' + $made.ReturnValue) }
Start-Sleep -Seconds 3
(Invoke-WebRequest -UseBasicParsing -TimeoutSec 10 ('http://127.0.0.1:' + $req.port + '/')).Content
'PID ' + $made.ProcessId
'''
WINDOWS_PB = r"Get-ChildItem -Path 'C:\Program Files\Paperboat\users\*\bin\pb.exe' | Select-Object -First 1 | ForEach-Object FullName"


class Windows:
    kind = os = 'windows'

    def __init__(self, host, device=None):
        if device is None:
            import winhost
            device = winhost.WindowsDevice(target=host, password='')  # key login (winpool); never the paired target's password
        self.host, self.device, self._pb, self._work = host, device, None, None

    def pb_exe(self):
        if not self._pb:
            _, out = self.device.ps('find pb', WINDOWS_PB, timeout=90)
            if not out.strip():
                raise AssertionError('pb is not installed on the Windows machine')
            self._pb = out.strip().splitlines()[-1]
        return self._pb

    def _run(self, exe, args, stdin=None, timeout=180):
        try:
            done = self.device.run([exe, *args], input=stdin, timeout=timeout)
        except subprocess.TimeoutExpired:
            return Result(124, '', 'timed out after %d s' % timeout)
        return Result(done.returncode, done.stdout, done.stderr)

    def pb(self, *args, stdin=None, timeout=180):
        return self._run(self.pb_exe(), list(args), stdin, timeout)

    def present(self):
        _, out = self.device.ps('look for Paperboat', WINDOWS_PRESENT, timeout=120)
        lines = [line.strip() for line in out.splitlines() if line.strip()]
        if not lines or lines[-1] != 'END':
            raise AssertionError('cannot look for Paperboat on the Windows machine')
        return lines[:-1]

    def hooks(self):
        return []  # pb installs no shell hook through these checks on Windows

    def drop_hooks(self):
        pass

    def apply_record(self):
        code, out, err = self.device.snippet('recipe apply record', WINDOWS_APPLY_RECORD, 90)
        return parse_record(Result(code, out.decode(errors='replace').replace('\r\n', '\n'), err.decode(errors='replace')), 'windows')

    def start_app(self, port, marker):
        code, out, err = self.device.snippet('local app', WINDOWS_START_APP, 90, port=port, marker=marker)
        text = out.decode(errors='replace')
        found = re.search(r'^PID (\d+)\s*$', text, re.M)
        if code or not found:
            raise AssertionError('the local app on port %d did not start: %s' % (port, Result(code, text, err.decode(errors='replace')).tail()))
        return int(found.group(1))

    def stop_app(self, pid):
        self.device.ps('stop the local app', 'Stop-Process -Id ([int]$req.pid) -Force -ErrorAction SilentlyContinue', pid=pid, timeout=60)

    def one_liner(self, url, variable, value, name, version=None, timeout=900):
        if variable not in VARIABLES:
            raise ValueError('unknown installer variable ' + variable)
        self._pb = None
        code, out, err = self.device.snippet('recipe one-liner', WINDOWS_ONE_LINER, timeout, url=url, variable=variable, value=value,
                                             name=name, version=version or '')
        return Result(code, out.decode(errors='replace'), err.decode(errors='replace'))

    def hostname(self):
        done = self._run(r'C:\Windows\System32\HOSTNAME.EXE', [], timeout=90)
        if done.code or not done.out.strip():
            raise AssertionError('hostname failed: ' + done.tail())
        return done.out.strip()

    def print_env_args(self, key):
        return ['powershell.exe', '-NoProfile', '-NonInteractive', '-Command', '[Console]::Write($env:' + key + ')']

    def write(self, filename, text):
        if self._work is None:
            _, out = self.device.ps('work directory', "$d = Join-Path $env:TEMP 'rcp-work'; New-Item -ItemType Directory -Force -Path $d | Out-Null; $d", timeout=90)
            self._work = out.strip().splitlines()[-1]
        path = self._work + '\\' + filename
        self.device.write_bytes(path, text.encode())
        return path

    def uninstall(self, wait=360):
        if self._work:
            self.device.ps('remove the work directory', 'Remove-Item -LiteralPath $req.path -Recurse -Force -ErrorAction SilentlyContinue', path=self._work, timeout=90)
            self._work = None
        if not self.present():
            return
        # the host name is the one pb compares
        done = self.pb('uninstall', '--confirmation', 'UNINSTALL PAPERBOAT', '--hostname', self.hostname(), '--json', timeout=420)
        self._pb = None
        receipt = json_data(done)
        # Windows ends the uninstall in a helper after the command returned: wait for its status before looking or reinstalling
        path = receipt.get('completion_status_path') if isinstance(receipt, dict) else None
        deadline, state = time.monotonic() + wait, None
        while path:
            try:
                state = json.loads(self.device.read_bytes(path).decode('utf-8-sig')).get('state')
            except Exception:  # noqa: BLE001 not written yet, or being replaced
                state = None
            if state == 'completed':
                return
            if state in ('failed', 'timed_out') or time.monotonic() >= deadline:
                raise AssertionError('the Windows uninstall helper did not complete: %s' % (state or 'no status'))
            time.sleep(3)


def json_data(result):
    """The data of a `pb … --json` answer (the envelope's, or the object itself)."""
    if result.code:
        raise AssertionError('pb exited %d: %s' % (result.code, result.tail()))
    try:
        value = json.loads(result.out.strip())
    except ValueError:
        try:
            value = json.loads(result.out.strip().splitlines()[-1])
        except (ValueError, IndexError):
            raise AssertionError('pb printed no JSON: ' + result.tail()) from None
    return value['data'] if isinstance(value, dict) and 'schema_version' in value else value
