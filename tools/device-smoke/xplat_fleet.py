#!/usr/bin/env python3
"""One Linux, one macOS and one Windows machine of the same account, driven from the controller (X rows).

  xplat_fleet.py up      install the build on each device that has none and pair it (token minted on the controller account)
  xplat_fleet.py status  pb version and machine state per device
  xplat_fleet.py down    revoke the three machines by exact id and uninstall

Devices (override with env): L = Docker container XPLAT_L_CT on XPLAT_L_HOST (user XPLAT_L_USER, default tester; XPLAT_L_UID, XPLAT_L_DOCKER), M = the Mac VM's `admin` user
(its machine `apple`, GUI session, keychain unlocked per command from ~/.pbmac/vm-login.pw), or with XPLAT_MAC_SSH=macpool-N a hosted macOS runner (machine `<prefix>-m`), W = winpool runner
SMOKE_WINDOWS_SSH. L and W are named `<XPLAT_PREFIX>-l|-w`. The account is the Mac user's: enrollment tokens are minted by
the Mac's own signed-in pb, so the three machines share one account. No secret is printed.
"""
import base64
import hashlib
import json
import os
import re
import shlex
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import winhost  # noqa: E402

PREFIX = os.environ.get('XPLAT_PREFIX', 'lanex')
L_HOST, L_CT = os.environ.get('XPLAT_L_HOST', 'hp'), os.environ.get('XPLAT_L_CT', 'laneX-l')
M_NAME = os.environ.get('XPLAT_M_NAME', 'apple')
MAC_SSH = str(Path.home() / '.pbmac/ssh.sh')
BUILD_DIR = Path(os.environ.get('XPLAT_BUILD_DIR', '.build/xplat'))


class Result:
    def __init__(self, rc, out, err):
        self.rc, self.out, self.err = rc, out, err

    @property
    def text(self):
        return self.out.decode('utf-8', 'replace')

    @property
    def errtext(self):
        return self.err.decode('utf-8', 'replace')


class Unix:
    """Linux container or macOS user: commands are `ssh … env K=V argv…` so argv reaches the device exactly."""

    def __init__(self, name, os_name, prefix, home, pb, pre=''):
        self.name, self.os, self.prefix, self.home, self.pb_path, self.pre = name, os_name, prefix, home, pb, pre
        self.alias = name

    def run(self, argv, input=None, env=None, timeout=120, cwd=None):
        words = ['env'] + ['%s=%s' % kv for kv in (env or {}).items()] + [str(a) for a in argv]
        script = ('cd %s && ' % shlex.quote(cwd) if cwd else '') + 'exec ' + ' '.join(shlex.quote(w) for w in words)
        command = self.prefix + ['bash', '-c', shlex.quote('export PATH=%s/.local/bin:$PATH; %s%s' % (self.home, self.pre, script))]
        try:
            done = subprocess.run(command, input=input, capture_output=True, timeout=timeout)
        except subprocess.TimeoutExpired:
            return Result(-1, b'', b'timed out')
        return Result(done.returncode, done.stdout, done.stderr)

    def sh(self, script, input=None, timeout=120):
        return self.run(['bash', '-c', script], input=input, timeout=timeout)

    def pb(self, *args, input=None, timeout=120, env=None):
        return self.run([self.pb_path, *args], input=input, timeout=timeout, env=env)

    def read(self, path):
        done = self.run(['cat', path])
        return done.out if done.rc == 0 else None

    def write(self, path, data, mode=None):
        script = 'mkdir -p "$(dirname %s)" && cat > %s' % (shlex.quote(path), shlex.quote(path))
        if mode:
            script += ' && chmod %s %s' % (mode, shlex.quote(path))
        done = self.sh(script, input=data)
        assert done.rc == 0, done.errtext
        return path

    def exists(self, path):
        return self.run(['test', '-e', path]).rc == 0

    def sha256(self, path):
        data = self.read(path)
        return None if data is None else hashlib.sha256(data).hexdigest()

    def remove(self, path):
        self.run(['rm', '-rf', path])


class Windows:
    os = 'windows'

    def __init__(self, name, target=None, pb_exe=None):
        self.name, self.alias = name, name
        self.pb_path = pb_exe or os.environ.get('XPLAT_W_PB', '')
        self.device = winhost.WindowsDevice(target=target or os.environ.get('SMOKE_WINDOWS_SSH', 'winpool-1'),
                                            pb_exe=self.pb_path or None, password=None)
        self.home = str(self.device.home())
        if not self.pb_path:  # the per-user install the build made
            found = self.sh("(Get-ChildItem 'C:\\Program Files\\Paperboat\\users\\*\\bin\\pb.exe' -ErrorAction SilentlyContinue | Select-Object -First 1).FullName")
            self.pb_path = self.device.pb_exe = found.text.strip()

    def run(self, argv, input=None, env=None, timeout=120, cwd=None):
        done = self.device.run([str(a) for a in argv], input=input, env=env, timeout=timeout, cwd=cwd, text=False)
        return Result(done.returncode, done.stdout, done.stderr)

    def sh(self, script, input=None, timeout=120):
        return self.run(['powershell.exe', '-NoProfile', '-NonInteractive', '-EncodedCommand',
                         base64.b64encode(script.encode('utf-16-le')).decode()], input=input, timeout=timeout)

    def pb(self, *args, input=None, timeout=120, env=None):
        return self.run([self.pb_path, *args], input=input, timeout=timeout, env=env)

    def read(self, path):
        return self.device.read_bytes(path) if self.device.exists(path) else None

    def write(self, path, data, mode=None):
        self.device.write_bytes(path, data)
        return path

    def exists(self, path):
        return self.device.exists(path)

    def sha256(self, path):
        return self.device.sha256(path) if self.device.exists(path) else None

    def remove(self, path):
        self.device.unlink(path)


def linux(name=None, ct=None):
    user, uid = os.environ.get('XPLAT_L_USER', 'tester'), os.environ.get('XPLAT_L_UID', '1001')
    docker = os.environ.get('XPLAT_L_DOCKER', 'docker').split()  # `sudo -n docker` on a host where the user is not in the docker group
    prefix = ['ssh', L_HOST, *docker, 'exec', '-i', '-u', user, '-e', 'HOME=/home/' + user,
              '-e', 'XDG_RUNTIME_DIR=/run/user/' + uid, ct or L_CT]
    return Unix(name or PREFIX + '-l', 'linux', prefix, '/home/' + user, '/home/%s/.local/bin/pb' % user)


def mac_unlock():
    password = (Path.home() / '.pbmac/vm-login.pw').read_text().strip()
    return 'security unlock-keychain -p %s "$HOME/Library/Keychains/login.keychain-db" >/dev/null 2>&1; ' % shlex.quote(password)


def mac(name=None):
    host = os.environ.get('XPLAT_MAC_SSH')  # a macpool-N runner instead of the Mac VM (its logins run in the GUI session)
    if host:
        return Unix(name or PREFIX + '-m', 'darwin', ['ssh', host], '/Users/runner', '/Users/runner/.local/bin/pb')
    return Unix(name or M_NAME, 'darwin', [MAC_SSH, 'vm'], '/Users/admin', '/Users/admin/.local/bin/pb', pre=mac_unlock())


def windows(name=None):
    return Windows(name or os.environ.get('XPLAT_W_NAME') or PREFIX + '-w')  # XPLAT_W_NAME: a runner paired next to the fleet's Victus record


def fleet(keys=('L', 'M', 'W')):
    """Only the requested devices are contacted: a dead runner must not stop a run that never uses it."""
    builders = {'L': linux, 'M': mac, 'W': windows}
    return {k: builders[k]() for k in keys}


def mint_token(kind, name=None, controller_dev=None):
    """A one-shot enrollment token from the account's signed-in pb (on the Mac); `kind` is `unix` or `windows`."""
    script = ('import json,re,subprocess,sys\n'
              'name,kind=sys.argv[1:3]\n'
              'out=subprocess.run([sys.argv[3],"machine","add","--name",name,"--json"],capture_output=True,text=True).stdout\n'
              'data=json.loads(out)["data"]\n'
              'cmd=data["windows_command" if kind=="windows" else "linux_macos_command"]\n'
              'url=re.search(r"\'(https?://[^\']+)\'",cmd).group(1)\n'
              'body=subprocess.run(["curl","-fsSL",url],capture_output=True,text=True).stdout\n'
              'print(re.search(r"ENROLLMENT_TOKEN\\W+([A-Za-z0-9]+)",body).group(1))\n')
    name = name or (os.environ.get('XPLAT_W_NAME') if kind == 'windows' else None) or PREFIX + '-' + ('w' if kind == 'windows' else 'l')
    ctl = controller_dev or controller()
    done = ctl.run(['python3', '-', name, kind, ctl.pb_path], input=script.encode(), timeout=90)
    token = done.text.strip()
    assert done.rc == 0 and re.fullmatch(r'[A-Za-z0-9]+', token), 'no enrollment token: ' + done.errtext[-200:]
    return token


def controller():
    """The device whose own signed-in pb mints tokens and lists machines: the Mac (account of the fleet of the Mac VM) or, with
    XPLAT_CONTROLLER=L, the Linux container (a fresh account that was signed in with an API token), or with XPLAT_CONTROLLER=hp
    the operator account's pb on hp."""
    if os.environ.get('XPE_CTL_CT'):  # a Docker container of the account that is never reinstalled (a build swap of L needs one)
        saved = globals()['L_CT']
        globals()['L_CT'] = os.environ['XPE_CTL_CT']
        try:
            return linux('ctl')
        finally:
            globals()['L_CT'] = saved
    if os.environ.get('XPLAT_CONTROLLER') == 'hp':  # the operator account's signed-in pb on hp: the fleet is that account's
        return Unix('hp', 'linux', ['ssh', 'hp'], '/home/anvit', '/home/anvit/.local/bin/pb')
    return linux() if os.environ.get('XPLAT_CONTROLLER') == 'L' else mac()


def controller_pb(*args, timeout=90):
    return controller().pb(*args, timeout=timeout)


def pair(dev, kind):
    """Pair an installed device under its machine name with a fresh token (written only to a private file on it)."""
    token_file = dev.home + ('\\.pb-xplat-token' if dev.os == 'windows' else '/.pb-xplat-token')
    dev.write(token_file, mint_token(kind).encode(), mode='600')
    if dev.os == 'windows':
        winhost.RemotePath(dev.device, token_file).enrollment_token_file()
    try:
        for attempt in range(2):
            done = dev.pb('pair', '--enrollment-token-file', token_file, '--name', dev.name, '--json', timeout=180)
            value = json.loads(done.text)
            if value.get('ok'):
                return value['data'].get('machine_id')
            if attempt or 'unfinished earlier enrollment' not in done.text:
                break
            host = dev.run(['hostname']).text.strip()  # a half-finished enrollment of an earlier try is discarded first
            dev.pb('reset', '--confirmation', 'RESET PAPERBOAT', '--hostname', host, '--enrollment-token-file', token_file, '--json')
        raise AssertionError('%s pair failed: %s' % (dev.name, done.text[-300:] + done.errtext[-300:]))
    finally:
        dev.remove(token_file)


def install(dev, binary):
    """Copy the build to the device and run its own `pb install`; returns the install receipt."""
    if dev.os == 'windows':
        target = dev.home + '\\pb-xplat.exe'
        subprocess.run(['scp', '-q', str(binary), '%s:%s' % (dev.device.target, target.replace('\\', '/'))], check=True, timeout=600)
        done = dev.run([target, 'install', '--json'], timeout=300)
    else:
        remote = dev.home + '/pb-xplat'
        dev.write(remote, Path(binary).read_bytes(), mode='755')
        done = dev.run([remote, 'install', '--json'], timeout=300)
    value = json.loads(done.text.strip().splitlines()[-1])
    assert value.get('ok'), 'install failed: ' + done.text[-300:] + done.errtext[-300:]
    if dev.os == 'windows':
        dev.pb_path = dev.device.pb_exe = value['data']['executable']
    return value['data']


def uninstall(dev):
    host = dev.run(['hostname']).text.strip()
    done = dev.pb('uninstall', '--confirmation', 'UNINSTALL PAPERBOAT', '--hostname', host, '--json', timeout=300)
    value = json.loads(done.text.strip().splitlines()[-1])
    assert value.get('ok'), 'uninstall failed: ' + done.text[-300:] + done.errtext[-300:]


def json_pb(dev, *args, timeout=120):
    """`pb <args> --json` and its data; raises when the output is not JSON."""
    done = dev.pb(*args, '--json', timeout=timeout)
    value = json.loads(done.text)
    return value.get('data', value) if isinstance(value, dict) else value


def machines():
    """The controller's machine list as {alias: record}."""
    listed = json.loads(controller_pb('machine', 'list', '--json').text)
    listed = listed.get('data', listed)
    return {m['alias']: m for m in listed['machines']}


def state(dev):
    done = dev.pb('machine', 'list', '--json')
    return done.rc


def login_controller(dev):
    """Sign the Linux controller in with the fresh account's API token (XPLAT_API_TOKEN_FILE, read here, never printed); a
    systemd container has no keyring, so the session goes to a file."""
    shown = json.loads(dev.pb('config', 'show', '--json').text)
    shown = shown.get('data', shown)
    dev.write(dev.home + '/.config/paperboat/config.json', json.dumps({'server_url': shown['server_url'], 'auth': {'allow_file_fallback': True}}).encode())
    token = Path(os.environ['XPLAT_API_TOKEN_FILE']).read_bytes().strip()
    done = dev.pb('login', '--api-token-file', '-', input=token + b'\n', timeout=120)
    assert done.rc == 0, 'login failed: ' + (done.text + done.errtext)[-300:]


def up(devices):
    """Install the build (BUILD_DIR/pb-<os>-<arch>) on every device, sign the Linux controller in, pair all."""
    files = {'linux': 'pb-linux-' + os.environ.get('XPLAT_L_ARCH', 'amd64'), 'darwin': 'pb-darwin-arm64', 'windows': 'pb-windows-amd64.exe'}
    for dev in devices.values():
        if dev.os != 'windows' or not dev.pb_path:
            install(dev, BUILD_DIR / files[dev.os])
        print('installed', dev.name, flush=True)
    assert os.environ.get('XPLAT_CONTROLLER') == 'L', 'up signs the Linux device in as the controller: set XPLAT_CONTROLLER=L'
    if 'L' in devices:
        login_controller(devices['L'])
    for key in ('L', 'M', 'W'):
        if key in devices:
            print('paired', devices[key].name, pair(devices[key], 'windows' if key == 'W' else 'unix'), flush=True)


def main():
    action = sys.argv[1] if len(sys.argv) > 1 else 'status'
    keys = sys.argv[sys.argv.index('--devices') + 1].split(',') if '--devices' in sys.argv else ['L', 'M', 'W']
    devices = {k: d for k, d in fleet().items() if k in keys}
    if action == 'up':
        return up(devices)
    if action == 'status':
        for key, dev in devices.items():
            done = dev.pb('version')
            print(key, dev.name, done.text.strip().splitlines()[1:2] if done.rc == 0 else done.errtext[-120:])
        known = machines()
        for dev in devices.values():
            print(dev.name, known.get(dev.name, {}).get('state', 'unpaired'))
    elif action == 'down':
        known = machines()
        for dev in devices.values():
            if dev.name in known:
                mid = known[dev.name]['id']
                token = json.loads(controller_pb('machine', 'revoke', mid, '--json').text)['data']['confirmation_token']
                controller_pb('machine', 'revoke', mid, '--json', '--confirm', token)
                print('revoked', dev.name, mid)
    else:
        raise SystemExit('usage: xplat_fleet.py up|status|down')


if __name__ == '__main__':
    main()
