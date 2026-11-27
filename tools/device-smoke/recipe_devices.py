"""The devices of the recipe checks (recipe_checks.py): a Linux container, a real Linux or macOS machine over SSH, a
Windows machine over SSH. Each runs pb, runs the installer one-liner with its secret on stdin (never in argv) and says
whether Paperboat is already on it. Everything printed back goes through redact()."""
import json
import re
import shlex
import subprocess

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


# pb as the installer leaves it on Linux and macOS
PB_FIND = ('P=; for c in "$HOME/.local/bin/pb" "/Library/PrivilegedHelperTools/Paperboat/users/u$(id -u)/bin/pb"; do '
           '[ -x "$c" ] && { P=$c; break; }; done; [ -n "$P" ] || { echo "pb is not installed" >&2; exit 127; }; ')
# anything of an install or a sign-in: a recipe check only ever starts on a machine with none of it
POSIX_PRESENT = ('for p in "$HOME/.local/bin/pb" "/usr/local/libexec/paperboat/users/u$(id -u)" '
                 '"/Library/PrivilegedHelperTools/Paperboat/users/u$(id -u)" "$HOME/.config/paperboat/config.json" '
                 '"$HOME/Library/Application Support/paperboat/config.json"; do [ -e "$p" ] && echo "$p"; done; '
                 'command -v pb; echo END')


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
        return ['ssh', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=20', '-o', 'ServerAliveInterval=30', self.host, run]

    def sh(self, script, stdin=None, timeout=300):
        return spawn(self.argv(script), stdin, timeout)

    def pb(self, *args, stdin=None, timeout=180):
        return self.sh(PB_FIND + '"$P" ' + ' '.join(shlex.quote(a) for a in args), stdin, timeout)

    def present(self):
        """What of Paperboat is already here (empty: a clean machine)."""
        done = self.sh(POSIX_PRESENT, timeout=90)
        lines = done.out.split()
        if done.code or not lines or lines[-1] != 'END':
            raise AssertionError('cannot look for Paperboat on the %s machine: %s' % (self.kind, done.tail()))
        return lines[:-1]

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
        done = self.sh(PB_FIND + '"$P" uninstall --confirmation "UNINSTALL PAPERBOAT" --hostname "$(hostname)" 2>&1; rm -rf "$HOME/rcp-work"', timeout=420)
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
  if (Test-Path -LiteralPath $p) { if ((Get-Item -LiteralPath $p).PSIsContainer) { Get-ChildItem -LiteralPath $p -Recurse -Filter pb.exe | ForEach-Object FullName } else { $p } }
}
Get-Service -Name 'Paperboat*' -ErrorAction SilentlyContinue | ForEach-Object Name
'END'
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

    def uninstall(self):
        if not self.present():
            return
        # run from outside the install directory; the host name is the one pb compares
        done = self.pb('uninstall', '--confirmation', 'UNINSTALL PAPERBOAT', '--hostname', self.hostname(), timeout=420)
        self._pb = None
        if done.code:
            raise AssertionError('pb uninstall: ' + done.tail())


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
