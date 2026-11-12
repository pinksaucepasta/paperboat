#!/usr/bin/env python3
"""X-28 and X-30 (plain ssh part): `ssh <machine>.<domain>` from a client of each OS to a target of each other OS.

  xpc_plainssh.py [--devices L,M,W] [--only config,ssh,env,hook,doctor]

config: the client's own ssh configuration (`~/.ssh/config`, `%USERPROFILE%\\.ssh\\config` on W) starts with the managed Include and
        the included file lists the target's alias with its own Host/Match block.
ssh:    plain `ssh <alias>.<domain> hostname` (BatchMode, no password prompt) returns what `pb ssh` returns.
env:    the ENV variables of xplat_env.py (one set, the same bytes everywhere) reach a plain `ssh <alias>.<domain> <command>` shell;
        compared by SHA-256 with what was set. Needs the fleet's ENV vault unlocked and the hook installed (xplat_env.setup does both).
hook:   `pb env hook <shell> --install` twice leaves one marker line; `--uninstall` restores the file the user had (bash, zsh on L and M;
        Windows PowerShell and PowerShell 7 profiles on W, BOM and CRLF kept).
doctor: `pb ssh doctor <alias> --json` is ready for every target.
Results: results/xpc-plainssh-<stamp>.json (`X-28:ssh:L>W`, `X-30:env:W>L`).
"""
import base64
import json
import shlex
import sys
import time
from itertools import permutations
from pathlib import Path

HERE = Path(__file__).parent
sys.path.insert(0, str(HERE))
import xplat_env  # noqa: E402
import xplat_fleet as fleet  # noqa: E402

results = {}


def domain(dev):
    config = (dev.read(dev.home + ('\\.ssh\\paperboat_config' if dev.os == 'windows' else '/.ssh/paperboat_config')) or b'').decode('utf-8', 'replace')
    for line in config.splitlines():
        if line.startswith('Host ') and '*' in line:
            return line.split('*.', 1)[1].strip()
    raise AssertionError('no managed ssh alias domain in %s: %s' % (dev.name, config[:120]))


def target_user(src, host):
    """The OS user the target's pb was set up by: the managed Match block names it (the first of its spellings)."""
    config = (src.read(src.home + ('\\.ssh\\paperboat_config' if src.os == 'windows' else '/.ssh/paperboat_config')) or b'').decode('utf-8', 'replace')
    for line in config.splitlines():
        if line.startswith('Match host %s user ' % host):
            return line.split('user ', 1)[1].strip().strip('"').split(',')[0]
    raise AssertionError('no managed Match block for %s' % host)


def plain(src, dst, *command, timeout=120):
    """ssh <user>@<alias>.<domain> <command> from src with the client's own ssh (the OS's OpenSSH) and the generated config: the user is
    the target's setup user (the Match block is for it), not the client's own login name."""
    host = '%s.%s' % (dst.alias, domain(src))
    argv = ['-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=accept-new', '%s@%s' % (target_user(src, host), host), *command]
    if src.os != 'windows':
        return src.run(['ssh', *argv], timeout=timeout)
    # Through the harness's piped process ssh.exe never returns (the proxy child keeps the pipes open); a user's console does not, so
    # run it as a plain process with its output in files, as a user's shell would.
    quoted = ','.join("'%s'" % ('"' + a.replace('"', '\\"') + '"').replace("'", "''") for a in argv)
    done = src.sh('''$e = Join-Path $env:TEMP 'xpc-ssh-err.txt'; $o = Join-Path $env:TEMP 'xpc-ssh-out.txt'; Remove-Item $e, $o -ErrorAction SilentlyContinue
$p = Start-Process ssh -ArgumentList %s -RedirectStandardError $e -RedirectStandardOutput $o -PassThru -NoNewWindow
if (-not $p.WaitForExit(%d)) { taskkill /PID $p.Id /T /F | Out-Null; 'XPC-TIMEOUT' }
'XPC-RC ' + $p.ExitCode; 'XPC-OUT'; [IO.File]::ReadAllText($o); 'XPC-ERR'; [IO.File]::ReadAllText($e); Remove-Item $e, $o -ErrorAction SilentlyContinue''' % (quoted, (timeout - 20) * 1000), timeout=timeout)
    text = done.text.replace('\r', '')
    head, _, rest = text.partition('XPC-OUT\n')
    out, _, err = rest.partition('XPC-ERR\n')
    code = int(head.split('XPC-RC ')[1].split()[0]) if 'XPC-RC ' in head else 255
    return fleet.Result(code if 'XPC-TIMEOUT' not in head else 124, out.encode(), err.encode())


def t_config(src, dst):
    path = src.home + ('\\.ssh\\config' if src.os == 'windows' else '/.ssh/config')
    text = (src.read(path) or b'').decode('utf-8', 'replace').lstrip('﻿')
    first = next((line for line in text.splitlines() if line.strip() and not line.startswith('#')), '')
    assert first.startswith('Include') and 'paperboat_config' in first, 'the ssh config does not start with the managed Include: %r' % first
    managed = (src.read(src.home + ('\\.ssh\\paperboat_config' if src.os == 'windows' else '/.ssh/paperboat_config')) or b'').decode('utf-8', 'replace')
    assert 'Host %s.%s' % (dst.alias, domain(src)) in managed, 'no block for %s in the managed file' % dst.alias
    return 'Include first in %s, alias block for %s' % (path.split('\\')[-1] if src.os == 'windows' else '~/.ssh/config', dst.alias)


def t_ssh(src, dst):
    done = plain(src, dst, 'hostname')
    via_pb = src.pb('ssh', dst.alias, '--', 'hostname', timeout=120)
    assert done.rc == 0 and done.text.strip().lower().split('.')[0] == via_pb.text.strip().lower().split('.')[0], \
        'plain ssh %r (rc %d, %s) vs pb ssh %r' % (done.text.strip(), done.rc, done.errtext[-200:], via_pb.text.strip())
    return 'plain ssh returned %s, as pb ssh did' % done.text.strip()


def t_env(src, dst):
    if dst.os == 'windows':
        script = "@'\n" + xplat_env.DUMP + "'@ | python -"
        command = ['powershell', '-EncodedCommand', base64.b64encode(script.encode('utf-16-le')).decode()]
    else:
        command = [shlex.join(['python3', '-c', xplat_env.DUMP.replace('\n', ';').replace('print(', 'print(', 1)])]
    done = plain(src, dst, *command, timeout=180)
    assert done.rc == 0, 'plain ssh env: rc %d %s %s' % (done.rc, done.errtext[-200:], done.text[-100:])
    xplat_env.compare(xplat_env.parse(done.text), 'plain ssh on ' + dst.name)
    return '%d ENV values equal (sha256) in a plain ssh command on %s' % (len(xplat_env.VALUES), dst.name)


HOOK_FILES = {'linux': (('bash', '.bashrc'), ('zsh', '.zshenv')), 'darwin': (('bash', '.bashrc'), ('zsh', '.zshenv'))}


def t_hook(dev):
    """Idempotent install, and uninstall that restores exactly the user's file."""
    notes = []
    if dev.os == 'windows':
        shells = [('powershell', 'Documents\\WindowsPowerShell\\profile.ps1'), ('pwsh', 'Documents\\PowerShell\\profile.ps1')]
    else:
        shells = HOOK_FILES[dev.os]
    sep = '\\' if dev.os == 'windows' else '/'
    for shell, rel in shells:
        path = dev.home + sep + rel.replace('\\', sep)
        original = (b'\xef\xbb\xbf# xpc user line\r\n$x = 1\r\n') if dev.os == 'windows' else b'# xpc user line\nexport XPC_USER=1\n'
        previous = dev.read(path)  # the machine's own file comes back whatever happens
        dev.write(path, original, mode='644')
        try:
            for _ in range(2):
                done = dev.pb('env', 'hook', shell, '--install', timeout=120)
                assert done.rc == 0, '%s hook install: %s' % (shell, (done.text + done.errtext)[-200:])
            installed = dev.read(path)
            assert installed.count(b'pb env') + installed.count(b'paperboat') >= 1 and installed.startswith(original.rstrip(b'\r\n')[:20]), 'user text lost: %r' % installed[:60]
            lines = [l for l in installed.splitlines() if b'paperboat' in l.lower() or b'pb env' in l]
            assert len(lines) == 1, '%s: %d hook lines after two installs: %r' % (shell, len(lines), lines)
            if dev.os == 'windows':
                assert installed.startswith(b'\xef\xbb\xbf') and b'\r\n' in installed and b'\n' not in installed.replace(b'\r\n', b''), 'BOM or CRLF not kept: %r' % installed[:80]
            done = dev.pb('env', 'hook', shell, '--uninstall', timeout=120)
            assert done.rc == 0, '%s hook uninstall: %s' % (shell, (done.text + done.errtext)[-200:])
            assert dev.read(path) == original, '%s: uninstall did not restore the file: %r' % (shell, dev.read(path)[:80])
            notes.append(shell)
        finally:
            dev.remove(path) if previous is None else dev.write(path, previous, mode='644')
    return 'hook install twice = one line, uninstall restores the user file byte for byte: %s' % ', '.join(notes)


def t_doctor(src, dst):
    data = fleet.json_pb(src, 'ssh', 'doctor', dst.alias)
    assert data.get('transport') == 'ready' and data.get('openssh', {}).get('ready') and data.get('target', {}).get('state') == 'ready', json.dumps(data)[:300]
    return 'pb ssh doctor ready (OpenSSH %s)' % data['openssh'].get('version', '?')[:30]


PAIR = [('X-28', 'config', t_config), ('X-28', 'ssh', t_ssh), ('X-30', 'env', t_env), ('X-28', 'doctor', t_doctor)]


def check(row, name, pair, action):
    key = '%s:%s:%s' % (row, name, pair)
    started = time.time()
    try:
        detail = action()
        results[key] = {'status': 'PASS', 'detail': detail or ''}
    except Exception as error:  # noqa: BLE001
        results[key] = {'status': 'FAIL', 'detail': str(error)[:700]}
    print('%s %s %ds %s' % (results[key]['status'], key, time.time() - started, results[key]['detail'][:300]), flush=True)


def main():
    argv = sys.argv[1:]
    keys = argv[argv.index('--devices') + 1].split(',') if '--devices' in argv else ['L', 'M', 'W']
    only = set(argv[argv.index('--only') + 1].split(',')) if '--only' in argv else set()
    devices = {k: d for k, d in fleet.fleet().items() if k in keys}
    if not only or 'env' in only:
        xplat_env.setup(devices)
    for a, b in permutations(devices, 2):
        for row, name, test in PAIR:
            if not only or name in only:
                check(row, name, '%s>%s' % (a, b), lambda: test(devices[a], devices[b]))
    for key, dev in devices.items():
        if not only or 'hook' in only:
            check('X-30', 'hook', key, lambda: t_hook(dev))
    out = HERE / 'results' / ('xpc-plainssh-%s.json' % time.strftime('%Y%m%dT%H%M%SZ', time.gmtime()))
    out.parent.mkdir(exist_ok=True)
    out.write_text(json.dumps({'check': 'xpc-plainssh', 'devices': keys, 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
