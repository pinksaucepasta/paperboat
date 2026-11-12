#!/usr/bin/env python3
"""X-31: the commands Paperboat prints for a machine of another OS, each pasted into the shell of the OS it is for.

  xplat_commands.py [--devices L,M,W] [--only dialog,addjson,install,wrong,tunnel]

Needs fresh machines (nothing installed) and a controller: a pb signed in to the account (never paired). Environment:
  XPE_CTL_CT   Docker container (on XPLAT_L_HOST) of the controller;  XPLAT_L_CT  the fresh Linux container (target L)
  XPLAT_MAC_SSH, SMOKE_WINDOWS_SSH  the fresh Mac / Windows runner;  XPLAT_PREFIX  machine name prefix (`<prefix>-l|-m|-w`)
  ORIGIN, STATE, PW_MODULE, PW_CHROMIUM  the live console and the account's session (console/scripts/run-console-check.mjs)
  XPE_BUILD_DIR  directory with the build under test (pb-linux-amd64, pb-darwin-arm64, pb-windows-amd64.exe)
Steps per target OS T, in order, each its own result `X-31:<step>:<T>` (or `A>B`, the command printed on A, pasted on B):
  wrong     the other platform's command pasted on the fresh T fails with the platform message and leaves nothing behind
  dialog    the console's Add machine dialog (check-add-machine.js on the live console) shows the T command, the machine
            runs it and the dialog turns into "Machine connected"
  addjson   `pb machine add --json` run on T names a command for every platform, each of the published shape
  install   a command printed by `pb machine add` on A installs and pairs B (B uninstalled first)
  tunnel    the `pb tunnel create` line the console prints runs verbatim on T
Nothing secret is printed: commands are matched against their shape and masked before any output.
"""
import json
import os
import re
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

HERE = Path(__file__).parent
sys.path.insert(0, str(HERE))
import xplat_fleet as fleet  # noqa: E402
from xplat_tunnel import confirm_delete  # noqa: E402

results = {}
PREFIX = os.environ.get('XPLAT_PREFIX', 'xpe')
POSIX_SHAPE = re.compile(r"^curl -fsSL '(https://[^'?]+/install)\?p=[A-Za-z0-9_.-]+' \| bash$")
PS_SHAPE = re.compile(r"""^powershell -c "iex \(irm '(https://[^'?]+/install\.ps1)\?p=[A-Za-z0-9_.-]+'\)"$""")
GIT_BASH = r'C:\Program Files\Git\bin\bash.exe'
BUILD = Path(os.environ.get('XPE_BUILD_DIR', '/home/ubuntu/pb-wt/xe-build/dev-9864c05bd'))
BINARIES = {'L': 'pb-linux-amd64', 'M': 'pb-darwin-arm64', 'W': 'pb-windows-amd64.exe'}
NAMES = {'L': PREFIX + '-l', 'M': PREFIX + '-m', 'W': PREFIX + '-w'}


def mask(text):
    return re.sub(r'(p=|token=|ENROLLMENT_TOKEN\W{1,3})[A-Za-z0-9_.-]{12,}', r'\1<token>', text)


def devices(keys):
    out = {}
    if 'L' in keys:
        out['L'] = fleet.linux(NAMES['L'])
    if 'M' in keys:
        out['M'] = fleet.mac(NAMES['M'])
    if 'W' in keys:
        out['W'] = fleet.windows(NAMES['W'])
    return out


def controller():
    saved = fleet.L_CT
    fleet.L_CT = os.environ['XPE_CTL_CT']
    try:
        return fleet.linux('xpe-ctl')
    finally:
        fleet.L_CT = saved


def printed(dev):
    """`pb machine add --json` on dev: (linux_macos_command, windows_command); raises when either is missing."""
    done = dev.pb('machine', 'add', '--json', timeout=90)
    data = json.loads(done.text).get('data', {})
    assert data.get('linux_macos_command') and data.get('windows_command'), 'pb machine add on %s: %s' % (dev.name, mask(done.text + done.errtext)[-300:])
    return data['linux_macos_command'], data['windows_command']


def paste(dev, command, shell=None):
    """The printed `command` run as a user pastes it into the shell of dev (`shell`: 'bash' on Windows pastes it into Git bash, 'pwsh' into pwsh)."""
    env = {'PAPERBOAT_MACHINE_NAME': dev.name}
    if dev.os == 'windows':
        target = dev.device.target
        if shell == 'bash':
            line = 'set PAPERBOAT_MACHINE_NAME=%s&& "%s" -c "%s"' % (dev.name, GIT_BASH, command)
        else:
            line = 'set PAPERBOAT_MACHINE_NAME=%s&& %s' % (dev.name, command)
        done = subprocess.run(['ssh', target, line], capture_output=True, timeout=900)
        return fleet.Result(done.returncode, done.stdout, done.stderr)
    if shell == 'pwsh':  # the printed `powershell -c "..."` with the PowerShell this OS has
        command = re.sub(r'^powershell', 'pwsh', command)
    return dev.run(['bash', '-c', command], env=env, timeout=900)


def leftovers(dev):
    """Product paths on a machine that must not exist before an install (or after a refused one)."""
    if dev.os == 'windows':
        script = ("$p=@('C:\\Program Files\\Paperboat','$env:LOCALAPPDATA\\Paperboat','$env:APPDATA\\Paperboat','$env:USERPROFILE\\.local\\bin\\pb.exe');"
                  "$p | ForEach-Object { $e=$ExecutionContext.InvokeCommand.ExpandString($_); if (Test-Path $e) { $e } }; "
                  "if (Get-Service -Name Paperboat* -ErrorAction SilentlyContinue) { 'service' }")
        return dev.sh(script).text.split()
    script = ('for p in "$HOME/.local/bin/pb" "$HOME/.config/paperboat" "$HOME/.local/state/paperboat" "$HOME/Library/Application Support/Paperboat" '
              '/usr/local/libexec/paperboat /Library/Paperboat; do [ -e "$p" ] && echo "$p"; done; '
              'ls /etc/systemd/system 2>/dev/null | grep -i paperboat; ls "$HOME/Library/LaunchAgents" /Library/LaunchDaemons 2>/dev/null | grep -i paperboat; true')
    return [x for x in dev.sh(script).text.splitlines() if x]


def machine_record(ctl, name):
    listed = fleet.json_pb(ctl, 'machine', 'list', '--q', name)['machines']
    return next((m for m in listed if m['alias'] == name), None)


def wait_online(ctl, name, seconds=240):
    end = time.time() + seconds
    while time.time() < end:
        record = machine_record(ctl, name)
        if record and record.get('state') == 'online':
            return record
        time.sleep(5)
    raise AssertionError('%s did not show online within %ds' % (name, seconds))


def uninstall(dev, ctl):
    """Back to a fresh machine: `pb uninstall`, then the dead record is revoked by exact id."""
    record = machine_record(ctl, dev.name)
    if dev.exists(dev.pb_path):
        fleet.uninstall(dev)
    if record:
        token = json.loads(ctl.pb('machine', 'revoke', record['id'], '--json').text)['data']['confirmation_token']
        ctl.pb('machine', 'revoke', record['id'], '--json', '--confirm', token)


def check(name, pair, action):
    key = 'X-31:%s:%s' % (name, pair)
    started = time.time()
    try:
        detail = action()
        results[key] = {'status': 'PASS', 'detail': mask(detail or '')}
        print('PASS %s %ds %s' % (key, time.time() - started, mask(detail or '')), flush=True)
    except Exception as error:  # noqa: BLE001
        results[key] = {'status': 'FAIL', 'detail': mask(str(error))[:600]}
        print('FAIL %s %ds: %s' % (key, time.time() - started, mask(str(error))[:600]), flush=True)


# --- wrong platform -------------------------------------------------------------------------------------------------------
def t_wrong(ctl, dev):
    posix, powershell = printed(ctl)
    assert not leftovers(dev), 'the machine is not fresh: %r' % leftovers(dev)
    if dev.os == 'windows':
        done = paste(dev, posix, shell='bash')
        want = 'only macOS and Linux are supported'
        command_kind = 'the POSIX command in Git bash'
    else:
        done = paste(dev, powershell, shell='pwsh')
        want = 'this command is for Windows'
        command_kind = 'the PowerShell command in pwsh'
        if done.rc == 127 or 'not found' in done.errtext:
            raise AssertionError('no PowerShell on this OS to paste the Windows command into: ' + mask(done.errtext)[-200:])
    text = mask(done.text + done.errtext)
    assert done.rc != 0 and want in text, '%s: rc %d without the platform message %r: %s' % (command_kind, done.rc, want, text[-300:])
    after = leftovers(dev)
    assert not after, '%s left %r behind' % (command_kind, after)
    return '%s stopped with "%s", nothing left behind' % (command_kind, want)


# --- the console dialog ---------------------------------------------------------------------------------------------------
class Sink(BaseHTTPRequestHandler):
    body = None

    def do_POST(self):
        Sink.body = self.rfile.read(int(self.headers.get('content-length', 0))).decode()
        self.send_response(204)
        self.send_header('access-control-allow-origin', '*')
        self.end_headers()

    do_OPTIONS = do_POST

    def log_message(self, *a):
        pass


def t_dialog(ctl, dev):
    """check-add-machine.js on the live console with a loopback sink: the command the dialog shows (token inside) goes to the sink and
    is pasted on the fresh machine; the dialog must turn into 'Machine connected'."""
    assert not leftovers(dev), 'the machine is not fresh: %r' % leftovers(dev)
    server = HTTPServer(('127.0.0.1', 0), Sink)
    Sink.body = None
    threading.Thread(target=server.serve_forever, daemon=True).start()
    script = HERE.parent.parent / 'console/scripts'
    glob = json.dumps({'__connect': 'http://127.0.0.1:%d/' % server.server_port, '__connectWindows': dev.os == 'windows', '__connectWait': 480000})
    check_run = subprocess.Popen(['nice', '-n', '15', 'node', str(script / 'run-console-check.mjs'), str(script / 'check-add-machine.js'), '/', glob],
                                 stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, env=os.environ)
    try:
        end = time.time() + 180
        while Sink.body is None and time.time() < end and check_run.poll() is None:
            time.sleep(1)
        assert Sink.body, 'the dialog never produced a command: ' + (check_run.communicate(timeout=60)[0] if check_run.poll() is not None else '')[-400:]
        command = Sink.body
        shape = PS_SHAPE if dev.os == 'windows' else POSIX_SHAPE
        assert shape.match(command), 'the command shown for %s has the wrong shape: %s' % (dev.os, mask(command)[:200])
        done = paste(dev, command)
        assert done.rc == 0, 'the pasted command failed (rc %d): %s' % (done.rc, mask(done.text + done.errtext)[-400:])
        output = check_run.communicate(timeout=600)[0]
    finally:
        server.shutdown()
        if check_run.poll() is None:
            check_run.kill()
    steps = [line for line in output.splitlines() if line.startswith(('PASS', 'FAIL'))]
    failed = [line for line in steps if line.startswith('FAIL')]
    assert steps and not failed, 'dialog check: %s' % mask(' | '.join(failed or output.splitlines()[-3:]))[:500]
    return '%d/%d dialog steps passed; the %s command pasted on %s installed and paired it (dialog: Machine connected)' % (
        len(steps), len(steps), 'PowerShell' if dev.os == 'windows' else 'POSIX', dev.os)


# --- pb machine add --------------------------------------------------------------------------------------------------------
def t_addjson(ctl, dev):
    posix, powershell = printed(dev)
    assert POSIX_SHAPE.match(posix), 'POSIX command shape: ' + mask(posix)[:200]
    assert PS_SHAPE.match(powershell), 'PowerShell command shape: ' + mask(powershell)[:200]
    return 'pb machine add --json on %s names the POSIX and the PowerShell command, both of the published shape' % dev.os


def t_install(ctl, src, dst):
    """The command printed on src for dst's platform installs and pairs dst (dst uninstalled first, its old record revoked)."""
    uninstall(dst, ctl)
    assert not leftovers(dst), 'uninstall left %r' % leftovers(dst)
    posix, powershell = printed(src)
    done = paste(dst, powershell if dst.os == 'windows' else posix)
    assert done.rc == 0, 'the command printed on %s failed on %s (rc %d): %s' % (src.os, dst.os, done.rc, mask(done.text + done.errtext)[-400:])
    wait_online(ctl, dst.name)
    return 'command printed on %s installed and paired %s (online)' % (src.name, dst.name)


# --- tunnel ----------------------------------------------------------------------------------------------------------------
def t_tunnel(ctl, dev):
    """The exact line the console builds (components/tunnels/new-tunnel.tsx) runs verbatim in the OS's own shell."""
    name, port = 'xpe-cmd-%s' % dev.os[:3], 18931
    line = 'pb tunnel create %s --port %d' % (name, port)
    src = (HERE.parent.parent / 'console/components/tunnels/new-tunnel.tsx').read_text()
    assert '`pb tunnel create ${nameValue} --port ${port.value || "3000"}`' in src, 'the console no longer builds the tunnel line this check mirrors'
    done = dev.pb(*line.split()[1:], '--json', timeout=120)
    assert done.rc == 0, 'the printed line failed on %s: %s' % (dev.os, mask(done.errtext + done.text)[-300:])
    confirm_delete(dev, name)
    return 'the line "%s" ran verbatim on %s (no quoting or path in it: a name slug and a port)' % (line, dev.os)


def main():
    argv = sys.argv[1:]
    keys = argv[argv.index('--devices') + 1].split(',') if '--devices' in argv else ['L', 'M', 'W']
    only = set(argv[argv.index('--only') + 1].split(',')) if '--only' in argv else set()
    devs = devices(keys)
    ctl = controller()

    def want(step):
        return not only or step in only
    for key, dev in devs.items():
        if want('wrong'):
            check('wrong', key, lambda: t_wrong(ctl, dev))
        if want('dialog'):
            check('dialog', key, lambda: t_dialog(ctl, dev))
    for key, dev in devs.items():
        if want('addjson'):
            check('addjson', key, lambda: t_addjson(ctl, dev))
        if want('tunnel'):
            check('tunnel', key, lambda: t_tunnel(ctl, dev))
    if want('install'):
        for a_key, src in devs.items():
            for b_key, dst in devs.items():
                if a_key != b_key:
                    check('install', '%s>%s' % (a_key, b_key), lambda: t_install(ctl, src, dst))
    stamp = time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())
    out = HERE / 'results' / ('xplat-commands-%s.json' % stamp)
    out.write_text(json.dumps({'check': 'xplat-commands', 'devices': keys, 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
