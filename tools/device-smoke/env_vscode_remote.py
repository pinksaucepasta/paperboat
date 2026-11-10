#!/usr/bin/env python3
"""F16-04: VS Code Remote-SSH's server sees ENV loaded by `pb env hook`.

  env_vscode_remote.py SSH_HOST

Headless stand-in for the Remote-SSH extension: like it, start the real VS Code server through a
non-interactive `ssh HOST bash` with the launch script on stdin, then read the running server's
/proc/<pid>/environ. Terminals, tasks and the extension host inherit that environment. A server
started before the hook was installed keeps its old environment, which is the documented reload
step: "Remote-SSH: Kill VS Code Server on Host", then reconnect. The probe value is compared by
SHA-256, never printed. Cleans up the probe, the hook, the server and its files on every exit.

A macOS host is detected over ssh (`uname -s`). It has no /proc, so the server's environment is read with
`ps eww`; its login shell is zsh, so the hook is the zsh one (~/.zshenv); the server is the darwin-arm64
build; hashes use shasum. Over ssh the host's login keychain must already be unlocked for the ENV vault.

  env_vscode_remote.py SSH_HOST [--password-file FILE_ON_THE_DEVICE]     with PB_SMOKE_WINDOWS_PB set (winhost.py)

On a Windows device the hook is PowerShell's, so the stand-in is Remote-SSH on a host whose shell is PowerShell: the
real win32-x64 server (a zip) is started by `ssh HOST powershell ...` with the profile loaded, and that ssh session
stays open while the server runs (Windows OpenSSH ends a session's processes with it). Windows has no /proc and no
tool that shows another process's environment, so the server reports its own: NODE_OPTIONS preloads a few lines into
the server's node process, which writes the SHA-256 of the probe in its process.env to a file named by its pid; the
check then confirms that pid is the running server-main.js process.
"""
import contextlib
import hashlib
import json
import secrets
import subprocess
import sys
import time

from env_ssh_hook import (PS_SHOW, UNSET_MAC, account, arguments, host_is_mac, probe_hash, windows_cleanup, windows_pb,
                          windows_shell, windows_unlock, windows_unset)

PB = '$HOME/.local/bin/pb'
PROBE = 'PB_ENV_HOOK_PROBE'
DIR = '$HOME/.lanej-vscode-server'
# `pb env unset` previews first and needs the preview's confirmation code.
UNSET = (f'c=$({PB} env unset {PROBE} --json 2>/dev/null | python3 -c "import json,sys;'
         f'print(json.load(sys.stdin).get(\'data\',{{}}).get(\'confirmation_token\',\'\'))");'
         f' [ -n "$c" ] && {PB} env unset {PROBE} --json --confirm "$c" >/dev/null 2>&1')
SERVER_URL = 'https://update.code.visualstudio.com/latest/server-linux-x64/stable'
SERVER_URL_MAC = SERVER_URL.replace('server-linux-x64', 'server-darwin-arm64')

# What Remote-SSH does on the host: fetch the server once, start it detached, report its pid.
LAUNCH = f'''
set -e
mkdir -p {DIR}
if [ ! -x {DIR}/bin/code-server ]; then
  curl -fsSL '{SERVER_URL}' | tar -xz -C {DIR} --strip-components 1
fi
nohup {DIR}/bin/code-server --start-server --host=127.0.0.1 --port=0 --without-connection-token \\
  --accept-server-license-terms --telemetry-level off >{DIR}/server.log 2>&1 &
echo PID $!
'''

# The darwin server archive carries no exec bits: mark the launcher and node executable after unpacking.
LAUNCH_MAC = LAUNCH.replace(SERVER_URL, SERVER_URL_MAC).replace(
    '--strip-components 1\n', '--strip-components 1\n  chmod +x {DIR}/bin/code-server {DIR}/node\n'.replace('{DIR}', DIR))

# Hash of the probe in the server's own environment (or "absent").
READ = f'''pid=$1
for i in $(seq 50); do [ -r /proc/$pid/environ ] && grep -q "listening\\|Extension host agent" {DIR}/server.log && break; sleep 0.2; done
v=$(tr '\\0' '\\n' </proc/$pid/environ | sed -n 's/^{PROBE}=//p')
[ -n "$v" ] && printf %s "$v" | sha256sum | cut -d' ' -f1 || echo absent
'''

# The same without /proc (macOS): `ps eww` appends a process's environment to its command line.
READ_MAC = f'''pid=$1
for i in $(seq 50); do kill -0 $pid 2>/dev/null && grep -Eq "listening|Extension host agent" {DIR}/server.log && break; sleep 0.2; done
v=$(ps eww -p $pid | tr ' ' '\\n' | sed -n 's/^{PROBE}=//p')
[ -n "$v" ] && printf %s "$v" | shasum -a 256 | cut -d' ' -f1 || echo absent
'''


def ssh(host, command, stdin=None, timeout=300):
    args = ['ssh', '-o', 'BatchMode=yes', '-T', host] + ([command] if command else [])
    return subprocess.run(args, input=stdin, capture_output=True, text=True, timeout=timeout)


def must(result, step):
    if result.returncode:
        raise RuntimeError(f'{step} failed (exit {result.returncode})')
    return result.stdout


def start_server(host, launch=LAUNCH):
    out = must(ssh(host, 'bash', stdin=launch), 'start VS Code server over ssh bash')
    return next(line.split()[1] for line in out.splitlines() if line.startswith('PID '))


def server_probe(host, pid, read=READ):
    return must(ssh(host, f'bash -s {pid}', stdin=read), 'read server environment').strip()


def wait_hook_sees_probe(host, want, seconds=90, sha='sha256sum'):
    """The runtime serves a shell's ENV from a 60 s cache: wait until a new shell loads the probe just set."""
    show = f'printf %s "${PROBE}" | {sha} | cut -d" " -f1'
    end = time.monotonic() + seconds
    while must(ssh(host, show), 'read probe in a new shell').strip() != want:
        assert time.monotonic() < end, 'new shells did not load the probe within the hook cache time'
        time.sleep(3)


def stop_server(host, pid):
    ssh(host, f'kill {pid} 2>/dev/null; pkill -f "{DIR}" 2>/dev/null; true')


# ---- a Windows device ---------------------------------------------------------------------------------------------

SERVER_URL_WINDOWS = SERVER_URL.replace('server-linux-x64', 'server-win32-x64')  # a zip
WINDOWS_DIR = '.lanej-vscode-server'  # in the user's profile folder, as DIR
PRELOAD_NAME = 'pb-env-probe.cjs'  # .cjs: the server's package.json makes .js an ES module
# Runs first in every node process the launch starts; only the server itself (server-main.js) writes its answer.
PRELOAD = """const fs = require('fs'), path = require('path'), crypto = require('crypto');
if (process.argv.some((a) => /server-main\\.js$/.test(a))) {
  const v = process.env.%s;
  fs.writeFileSync(path.join(__dirname, 'env-' + process.pid + '.txt'),
    v ? crypto.createHash('sha256').update(v).digest('hex') : 'absent');
}
""" % PROBE
# What Remote-SSH does on the host: fetch the server once. tar.exe (bsdtar, part of Windows) unpacks the zip.
FETCH = ("if(Test-Path -LiteralPath (Join-Path $req.dir 'node.exe')){exit 0}\n"
         "[void](New-Item -ItemType Directory -Force -Path $req.dir);$z=Join-Path $req.dir 'server.zip'\n"
         "[Net.ServicePointManager]::SecurityProtocol=[Net.SecurityProtocolType]::Tls12\n"
         "Invoke-WebRequest -UseBasicParsing -Uri $req.url -OutFile $z\n"
         "& tar.exe -xf $z -C $req.dir --strip-components 1\n"
         "if($LASTEXITCODE -ne 0){throw 'unpacking the VS Code server failed'}\n"
         "Remove-Item -LiteralPath $z -Force")
# Typed by nobody: the command of the hooked PowerShell that keeps the server in its foreground. NODE_OPTIONS takes
# the preload path in double quotes with forward slashes (a backslash is an escape there).
LAUNCH_WINDOWS = ("$d=Join-Path $env:USERPROFILE '%s'\n"
                  "$env:NODE_OPTIONS='--require \"'+((Join-Path $d '%s') -replace '\\\\','/')+'\"'\n"
                  "& (Join-Path (Join-Path $d 'bin') 'code-server.cmd') --start-server --host=127.0.0.1 --port=0 "
                  "--without-connection-token --accept-server-license-terms --telemetry-level off "
                  "*> (Join-Path $d 'server.log')" % (WINDOWS_DIR, PRELOAD_NAME))


def start_server_windows(device, host):
    """The ssh session of a hooked PowerShell with the server in its foreground."""
    import base64
    command = ('powershell -NonInteractive -OutputFormat Text -EncodedCommand '
               + base64.b64encode(LAUNCH_WINDOWS.encode('utf-16-le')).decode())
    return subprocess.Popen(device.ssh_prefix(host, command), stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                            stderr=subprocess.DEVNULL, env=device.ssh_environment())


def server_probe_windows(device, directory, session, seconds=180):
    """Hash of the probe in the running server's own environment (or "absent")."""
    end = time.monotonic() + seconds
    while True:
        names = device.files(directory, 'env-*.txt')
        if names and device.count_in_files(directory, 'server.log', 'Extension host agent'):
            break
        if session.poll() is not None:
            raise RuntimeError('the VS Code server ended before it was listening')
        if time.monotonic() > end:
            raise RuntimeError('the VS Code server was not listening within %d s' % seconds)
        time.sleep(2)
    pid = int(names[0][len('env-'):-len('.txt')])
    if len(names) != 1 or pid not in [int(found['ProcessId']) for found in device.processes(r'server-main\.js')]:
        raise RuntimeError('the environment was not reported by the one running server process')
    return (directory / names[0]).read_text().strip()


def stop_server_windows(device, directory, session):
    if session is not None and session.poll() is None:
        session.terminate()
        session.wait(timeout=30)
    for found in device.processes(WINDOWS_DIR.replace('.', r'\.')):
        with contextlib.suppress(Exception):  # a child may be gone with its parent's tree
            device.stop(found['ProcessId'])
    for name in device.files(directory, 'env-*.txt'):
        (directory / name).unlink()


def main_windows(host, device, password_file):
    value = secrets.token_urlsafe(24)
    want = hashlib.sha256(value.encode()).hexdigest()
    report = {'host': host}
    directory = device.home() / WINDOWS_DIR
    sessions = []
    windows_unlock(password_file)
    try:
        must(windows_pb('env', 'set', PROBE, '--value-stdin', stdin=value), 'env set')
        device.ps('fetch the VS Code server', FETCH, timeout=900, dir=str(directory), url=SERVER_URL_WINDOWS)
        (directory / PRELOAD_NAME).write_text(PRELOAD)
        sessions.append(start_server_windows(device, host))
        must(windows_pb('env', 'hook', 'powershell', '--install'), 'hook install')
        end = time.monotonic() + 90  # the runtime serves a shell's ENV from a 60 s cache
        while probe_hash(must(windows_shell(device, host, PS_SHOW), 'read probe in a new shell')) != want:
            assert time.monotonic() < end, 'new shells did not load the probe within the hook cache time'
            time.sleep(3)
        stale = server_probe_windows(device, directory, sessions[-1])
        report['server_started_before_hook_has_probe'] = stale == want
        stop_server_windows(device, directory, sessions[-1])  # the documented reload step
        sessions.append(start_server_windows(device, host))
        report['server_after_reload_has_probe'] = server_probe_windows(device, directory, sessions[-1]) == want
        stop_server_windows(device, directory, sessions[-1])
        report['ok'] = report['server_after_reload_has_probe'] and not report['server_started_before_hook_has_probe']
    finally:
        windows_cleanup(*[lambda session=session: stop_server_windows(device, directory, session) for session in sessions],
                        lambda: stop_server_windows(device, directory, None), directory.unlink,
                        lambda: windows_pb('env', 'hook', '--uninstall'), windows_unset)
    report['status'] = 'PASS' if report.get('ok') else 'FAIL'  # what windows_checks.py reads
    print(json.dumps(report, sort_keys=True))
    return 0 if report.get('ok') else 1


def main():
    host, device, password_file = arguments()
    if device:
        return main_windows(host, device, password_file)
    value = secrets.token_urlsafe(24)
    want = hashlib.sha256(value.encode()).hexdigest()
    report = {'host': host}
    mac = host_is_mac(host)
    shell, launch, read, sha = (('zsh', LAUNCH_MAC, READ_MAC, 'shasum -a 256') if mac
                                else ('bash', LAUNCH, READ, 'sha256sum'))
    try:
        must(account(host, f'{PB} env set {PROBE} --value-stdin', stdin=value), 'env set')
        stale = start_server(host, launch)
        must(ssh(host, f'{PB} env hook {shell} --install'), 'hook install')
        wait_hook_sees_probe(host, want, sha=sha)
        report['server_started_before_hook_has_probe'] = server_probe(host, stale, read) == want
        stop_server(host, stale)  # the documented reload step
        fresh = start_server(host, launch)
        report['server_after_reload_has_probe'] = server_probe(host, fresh, read) == want
        stop_server(host, fresh)
        report['ok'] = report['server_after_reload_has_probe'] and not report['server_started_before_hook_has_probe']
    finally:
        ssh(host, f'pkill -f "{DIR}"; rm -rf {DIR}; {PB} env hook --uninstall >/dev/null 2>&1; true')
        account(host, UNSET_MAC if mac else UNSET)
    print(json.dumps(report, sort_keys=True))
    return 0 if report.get('ok') else 1


if __name__ == '__main__':
    sys.exit(main())
