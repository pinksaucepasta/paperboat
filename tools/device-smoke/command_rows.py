#!/usr/bin/env python3
"""Command-row checks (CHECKLIST Part B) for one signed-in Unix machine.

Every row checks: --help is accurate, the main use gives the right result, --json is valid where the
command supports it, invalid input and missing state give a short actionable error (no stack trace).
Rows that change the machine (machine-guard install/uninstall) run with --sudo-password-stdin.
Prints one receipt per row; exits 1 if any row failed. Needs the ENV vault unlocked for env rows.
"""
import argparse
import contextlib
import json
import os
from pathlib import Path, PureWindowsPath
import platform
import re
import subprocess
import sys
import tempfile

import winhost

ACCESS_ROWS = {'C002', 'C124', 'C125', 'C126', 'C139', 'C141'}
PB = winhost.pb_path()
BAD = re.compile(r'panic|goroutine|Traceback|runtime error|nil pointer')


DEVICE = winhost.device()  # the Windows device when PB_SMOKE_WINDOWS_PB is set: pb.exe and probes run there


def pb_run(*args, timeout=90, env=None):
    if DEVICE:
        return PB.run_remote(args, env=env, timeout=timeout)
    return subprocess.run([str(PB), *args], capture_output=True, text=True, timeout=timeout,
                          stdin=subprocess.DEVNULL, env=env)


def run(*args, timeout=90):
    result = pb_run(*args, timeout=timeout)
    return result.returncode, result.stdout, result.stderr


def sh(*args, timeout=60):
    result = subprocess.run(list(args), capture_output=True, text=True, timeout=timeout, stdin=subprocess.DEVNULL)
    return result.returncode, result.stdout + result.stderr


def scratch(name):
    return winhost.RemotePath(DEVICE, DEVICE.env('TEMP')) / name if DEVICE else Path('/tmp') / name


def size_of(path):
    return path.size() if DEVICE else path.stat().st_size


def private(path):
    # Windows keeps a file in the user's profile private by its inherited ACL; only Unix modes can be checked.
    return DEVICE is not None or path.stat().st_mode & 0o077 == 0


@contextlib.contextmanager
def sandbox_home():
    """A throwaway home for `config sync init`: (home, environment, make_path); never the real file."""
    if DEVICE:
        with DEVICE.temp_dir('pbsmoke-sync-init') as home:
            yield str(home), {'USERPROFILE': str(home), 'XDG_CONFIG_HOME': str(home / 'config')}, \
                lambda value: winhost.RemotePath(DEVICE, value)
        return
    with tempfile.TemporaryDirectory(prefix='pbsmoke-sync-init-') as home:
        home = os.path.realpath(home)  # macOS /var is a symlink; init refuses linked parents by design
        yield home, dict(os.environ, HOME=home, XDG_CONFIG_HOME=os.path.join(home, 'config')), Path


def sudo(password, *args):
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    import terminal_command
    result = terminal_command.run(['sudo', '-k', '--', str(PB), *args], password=password, timeout=240)
    return result.returncode, re.sub(r'\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x1b]*\x1b\\', '', result.stdout)


DARWIN = platform.system() == 'Darwin'


def guard_active():
    if DEVICE:
        return any(task['State'] in ('Running', 'Ready') for task in DEVICE.scheduled_tasks('\\Paperboat\\')
                   if task['TaskName'] == 'MachineGuard')
    if DARWIN:
        job = sh('sudo', '-n', 'launchctl', 'print', 'system/io.paperboat.machineguard')[1]
        return 'state = running' in job and 'deny-only' not in job  # an uninstalled guard keeps its deny-only job
    return sh('systemctl', 'is-active', 'paperboat-machineguard')[1].strip() == 'active'


def process_running(pattern):
    if DEVICE:
        return bool(DEVICE.processes(pattern))
    return sh('pgrep', '-f', pattern)[0] == 0


def guard_denies():
    """The cached-address protection that survives a guard uninstall."""
    if DARWIN:
        return 'block drop quick on ! lo0 inet from any to 127.100.0.0/16' in sh('sudo', '-n', 'pfctl', '-a', 'com.apple/000.paperboat-machineguard', '-s', 'rules')[1]
    return sh('systemctl', 'is-enabled', 'paperboat-machineguard-deny')[0] == 0


def daemon_unit_text():
    if DEVICE:  # the daemon's service command line, e.g. "pb.exe daemon __runtime-local-daemon ..."
        return ' '.join(item['PathName'] or '' for item in DEVICE.service_paths('PaperboatLocalDaemon*'))
    if DARWIN:
        return sh('launchctl', 'print', 'gui/%d/com.pinksaucepasta.paperboatd' % os.getuid())[1]
    return sh('systemctl', '--user', 'cat', 'paperboatd')[1]


class Row:
    def __init__(self, ident, command):
        self.ident, self.command, self.failures = ident, command.split(), []

    def expect(self, condition, message):
        if not condition:
            self.failures.append(message)

    def help(self):
        code, out, err = run(*self.command, '--help')
        self.expect(code == 0 and 'Usage:' in out and ' '.join(self.command) in out, '--help is missing usage or the command name')
        self.expect(not BAD.search(out + err), '--help printed a stack trace')

    def invalid(self, *extra, hint=None):
        """Invalid input: non-zero, short, actionable, no stack trace."""
        code, out, err = run(*self.command, *extra)
        text = out + err
        self.expect(code != 0, 'invalid input was accepted: ' + ' '.join(extra))
        self.expect(bool(text.strip()) and not BAD.search(text), 'invalid input gave no message or a stack trace')
        if hint:
            self.expect(re.search(hint, text) is not None, 'invalid input message lacks "%s"' % hint)

    def json_ok(self, *extra):
        code, out, err = run(*self.command, *extra, '--json')
        try:
            value = json.loads(out)
        except ValueError:
            self.failures.append('--json output is not JSON: ' + ' '.join(extra))
            return None
        self.expect(code == 0, '--json exit code %s: %s' % (code, ' '.join(extra)))
        if isinstance(value, dict) and 'ok' in value:
            self.expect(value['ok'] is True, '--json reported failure: ' + ' '.join(extra))
            return value.get('data', value)
        return value


def rows(args):
    out = []
    machine = args.machine

    r = Row('C143', 'pb env'.replace('pb ', '')); r.help()
    d = r.json_ok(); r.expect(d and 'commands' in d, 'pb env --json lists no commands')
    names = Row('C143', 'env list'); v = names.json_ok(); r.expect(v is not None and isinstance(v.get('names'), list), 'env list has no names array')
    r.failures += names.failures; r.invalid('bogus', hint='invalid|usage|Usage'); out.append(r)

    r = Row('C144', 'env grants'); r.help(); r.invalid('bogus')
    s = Row('C144', 'env grants sync'); v = s.json_ok(); r.expect(v and v.get('grants_synchronized') is True, 'grants sync did not synchronize'); r.failures += s.failures
    out.append(r)

    r = Row('C146', 'env host'); r.help(); r.invalid('bogus')
    s = Row('C146', 'env host show'); v = s.json_ok(); r.failures += s.failures
    r.expect(isinstance(v, dict) and {'published', 'applied', 'layers', 'machine_id'} <= set(v), 'env host show lacks delivery fields')
    r.expect(isinstance(v, dict) and v.get('machine_id', '').startswith('machine_'), 'env host show names no machine')
    out.append(r)

    r = Row('C150', 'env rotate cancel'); r.help(); r.invalid(hint='invalid|confirm|Usage')
    account = json.loads(run('auth', 'status', '--json')[1])['account']['id']
    code, o, e = run('env', 'rotate', 'cancel', '--confirm', 'CANCEL ENV ROTATE ' + account, '--json')
    r.expect('nothing' in (o + e).lower() or 'no rotation' in (o + e).lower() or '"ok":true' in o.replace(' ', ''),
             'cancel with no rotation in progress does not say there is nothing to cancel')
    out.append(r)

    r = Row('C152', 'env team'); r.help(); r.invalid('bogus')
    d = r.json_ok(); r.expect(d and {c['command'] for c in d['commands']} >= {'pb env team ' + x for x in ('create', 'grant', 'reset', 'revoke', 'rotate')}, 'env team --json misses subcommands')
    out.append(r)

    r = Row('C159', 'env vault'); r.help(); r.invalid('bogus')
    d = r.json_ok(); r.expect(d and len(d.get('commands', [])) >= 8, 'env vault --json misses subcommands')
    out.append(r)

    r = Row('C216', 'update'); r.help()
    r.invalid('--from', '/nonexistent/pb', hint='.')
    out.append(r)

    r = Row('C221', 'update status'); r.help()
    v = r.json_ok()
    installed = ''.join(line.split('Version ', 1)[1].strip() for line in run('version')[1].splitlines() if 'Version ' in line)
    r.expect(isinstance(v, dict) and 'settings' in v and installed and v.get('cli_version') == installed,
             'update status cli_version %r is not the installed version %r' % ((v or {}).get('cli_version'), installed))
    r.expect(isinstance(v, dict) and (not v.get('runtime_version') or v['runtime_version'] == installed),
             'update status runtime_version differs from the installed version')
    r.expect(isinstance(v, dict) and (not v.get('runtime_available') or v.get('runtime_state') == 'ready'),
             'update status says the runtime is available but not ready')
    out.append(r)

    r = Row('C217', 'update bundle'); r.help(); r.invalid(hint='output')
    target = scratch('pbsmoke-command-rows.pb-update')
    target.unlink(missing_ok=True)
    code, o, e = run('update', 'bundle', '--output', str(target), timeout=300)
    r.expect(code == 0 and target.exists() and size_of(target) > 1000 and 'pb update --from' in o + e, 'bundle was not saved with install instructions')
    r.expect(run('update', 'bundle', '--output', str(target))[0] != 0, 'bundle overwrote an existing output file')
    target.unlink(missing_ok=True)
    out.append(r)

    r = Row('C185', 'config sync init'); r.help()
    with sandbox_home() as (home, env, make_path):
        def init(*extra):
            result = pb_run('config', 'sync', *extra, '--json', timeout=60, env=env)
            return result.returncode, (json.loads(result.stdout or '{}').get('data') or {}), result.stdout + result.stderr
        _, where, _ = init('path')
        r186 = Row('C186', 'config sync path')
        located = make_path(where.get('path', '/nonexistent'))
        r186.expect(bool(where.get('path')) and str(located).startswith(home) and not located.exists(),
                    'config sync path is not inside the sandbox home or created the file')
        out.append(r186)
        code, made, _ = init('init')
        path = make_path(where.get('path', '/nonexistent'))
        r.expect(code == 0 and made.get('path') == str(path) and str(path).startswith(home), 'init did not create the file at its path')
        r.expect(path.exists() and private(path), 'the created file is missing or readable by others')
        before = path.read_bytes() if path.exists() else b''
        code, _, text = init('init')
        r.expect(code != 0 and not BAD.search(text) and path.read_bytes() == before, 'a second init overwrote the file or gave no clear error')
    out.append(r)

    r = Row('C263', 'account delete'); r.help()
    code, o, e = run('account', 'delete')
    r.expect(code == 0 and '/settings/account' in o + e, 'account delete does not print the console link')
    d = r.json_ok()
    r.expect(isinstance(d, dict) and str(d.get('console_url', '')).endswith('/settings/account'), 'account delete --json lacks console_url')
    r.expect(run('auth', 'status', '--json')[0] == 0, 'the account is no longer signed in after account delete')
    out.append(r)
    r = Row('C286', 'env names access'); r.help(); r.invalid('maybe', hint='on\\|off|use:')
    shown = r.json_ok()
    original = bool((shown or {}).get('names_listable', True)) if isinstance(shown, dict) else True
    r.expect(isinstance(shown, dict) and isinstance(shown.get('names_listable'), bool) and shown.get('workspace_id'), 'names access --json lacks names_listable and workspace_id')
    try:
        for state, word in ((not original, 'on' if not original else 'off'), (original, 'on' if original else 'off')):
            code, o, e = run('env', 'names', 'access', word, '--json')
            r.expect(code == 0 and json.loads(o or '{}').get('data', {}).get('names_listable') is state, 'names access %s did not set the state' % word)
            now = Row('C286', 'env names access').json_ok()
            r.expect(isinstance(now, dict) and now.get('names_listable') is state, 'names access does not show the state after %s' % word)
            names = run('env', 'names', '--json')
            if state:
                r.expect(names[0] == 0, 'env names failed while listing is allowed')
            else:
                r.expect(names[0] != 0 and 'turned off' in names[1] + names[2] and not BAD.search(names[1] + names[2]), 'env names was not refused with the turned-off message while access is off')
    finally:
        run('env', 'names', 'access', 'on' if original else 'off', '--json')  # leave the workspace as it was
    out.append(r)

    r = Row('C287', 'version'); r.help()
    code, o, e = run('version')
    code2, short, _ = run('--version')
    r.expect(code == 0 and code2 == 0 and o.strip() == short.strip() and 'Version ' in o, 'pb version differs from pb --version or prints no version')
    d = r.json_ok()
    r.expect(isinstance(d, dict) and d.get('version') and re.fullmatch(r'[0-9a-f]{7,40}', d.get('commit', '')) is not None
             and d['version'] in o, 'version --json lacks the version and source commit (7-40 hex) that the text shows')
    r.invalid('bogus')
    out.append(r)

    for ident, name, wanted in (
        ('C288', 'account', ('delete',)),
        ('C289', 'api-token', ('create', 'list', 'revoke')),
        ('C290', 'policy', ('get', 'set', 'test', 'explain', 'versions', 'restore', 'schema')),
        ('C291', 'team scim', ('status', 'setup', 'map')),
        ('C292', 'team sso', ('status', 'setup', 'enforce', 'exempt')),
        ('C294', 'team bucket', ('get', 'set', 'remove')),
    ):
        r = Row(ident, name); r.help(); r.invalid('bogus', hint='invalid|unknown|usage|Usage')
        d = r.json_ok()
        listed = {c.get('command') for c in (d or {}).get('commands', [])} if isinstance(d, dict) else set()
        r.expect(listed >= {'pb %s %s' % (name, sub) for sub in wanted}, '%s --json misses subcommands %s' % (name, sorted(set('pb %s %s' % (name, sub) for sub in wanted) - listed)))
        r.expect(run('auth', 'status', '--json')[0] == 0, 'no longer signed in after %s' % name)
        out.append(r)

    # Their main use (save, show without the secret, remove) is team_bucket_lifecycle.py; here: help and invalid input.
    for ident, name in (('C295', 'team bucket get'), ('C296', 'team bucket set'), ('C297', 'team bucket remove')):
        r = Row(ident, name); r.help(); r.invalid(hint='accepts|Usage|usage|team'); r.invalid('no-such-team-recb', hint='.')
        out.append(r)

    for ident, name in (('C218', 'update check'), ('C219', 'update download')):
        r = Row(ident, name); r.help()
        custom = run(*r.command, '--json')
        reply = custom[1] + custom[2]
        # On a custom build the documented answer is a clear refusal naming the way out.
        r.expect(custom[0] != 0 and 'custom build' in reply and ('--from' in reply or 'update --force' in reply),
                 'official-update command gave no clear answer on a custom build')
        out.append(r)

    r = Row('C228', 'config'); r.help(); r.invalid('bogus')
    v = Row('C228', 'config show').json_ok()
    absolute = (PureWindowsPath if DEVICE else Path)((v or {}).get('path', '')).is_absolute()  # the device's own path form
    r.expect(isinstance(v, dict) and v.get('server_url') and absolute, 'config show lacks server_url/path')
    out.append(r)

    r = Row('C236', 'config path'); r.help()
    shown = run('config', 'path')[1].strip()
    r.expect(shown == (v or {}).get('path') and (PureWindowsPath if DEVICE else Path)(shown).is_absolute(),
             'config path is not the absolute path config show reports')
    out.append(r)

    r = Row('C182', 'config status'); r.help()
    d = r.json_ok()
    r.expect(isinstance(d, dict) and d, 'config status --json is not a status object')
    out.append(r)

    r = Row('C022', 'auth'); r.help(); r.invalid('bogus')
    d = Row('C022', 'auth status').json_ok()
    r.expect(isinstance(d, dict) and d.get('signed_in') is True and d.get('account', {}).get('email'), 'auth status --json does not show the signed-in account')
    human = run('auth', 'status')[1]
    r.expect('@' in human, 'auth status shows no account')
    out.append(r)

    r = Row('C030', 'machine'); r.help(); r.invalid('bogus')
    d = Row('C030', 'machine list').json_ok()
    r.expect(isinstance(d, dict) and any(m['alias'] == machine and m['state'] == 'online' for m in d.get('machines', [])), 'machine list does not show this machine online')
    if args.offline_alias:
        code, o, e = run('exec', args.offline_alias, '--', 'true')
        r.expect(code != 0 and re.search('offline|disconnected|revoked|not found', o + e, re.I) is not None, 'exec to a disconnected machine gave no clear message')
    out.append(r)

    r = Row('C004', 'daemon'); r.help(); r.invalid('bogus')
    code, o, e = run('service', 'status')
    o += e
    r.expect(code == 0 and 'running' in o, 'service status does not report the daemon running')
    r.expect(process_running(r'pb(\.exe)?"? daemon' if DEVICE else r'pb daemon --config'), 'no daemon process is running')
    out.append(r)

    r = Row('C009', 'daemon run'); r.help()
    unit = daemon_unit_text()
    r.expect('daemon' in unit and ('program' in unit if DARWIN else 'daemon' in unit if DEVICE else 'ExecStart' in unit), 'user service does not start pb daemon')
    out.append(r)


    guard = Row('C005', 'daemon machine-guard'); guard.help(); guard.invalid('bogus')
    guard.expect(guard_active(), 'machine guard service is not active')
    out.append(guard)
    r = Row('C007', 'daemon machine-guard run'); r.help()
    r.expect(process_running('daemon machine-guard run'), 'machine guard run entrypoint is not running under supervision')
    out.append(r)
    install = Row('C006', 'daemon machine-guard install'); install.help()
    uninstall = Row('C008', 'daemon machine-guard uninstall'); uninstall.help()
    if args.sudo_password:
        code, o = sudo(args.sudo_password, 'daemon', 'machine-guard', 'install', '--json')
        install.expect(code == 0 and '"installed":true' in o.replace(' ', ''), 'install (repeat) did not report installed')
        install.expect(guard_active(), 'guard not active after install')
        code, o = sudo(args.sudo_password, 'daemon', 'machine-guard', 'uninstall', '--json')
        uninstall.expect(code == 0, 'uninstall failed: ' + o[-200:])
        uninstall.expect(not guard_active(), 'guard still active after uninstall')
        uninstall.expect(guard_denies(), 'cached-address protection was not retained')
        code, o = sudo(args.sudo_password, 'daemon', 'machine-guard', 'install', '--json')
        install.expect(code == 0 and guard_active(), 'reinstall after uninstall failed')
        install.expect(run('exec', machine, '--', 'true')[0] == 0, 'exec failed after guard reinstall')
    out += [install, uninstall]
    return out


def access_rows(args):
    out = []
    if args.peer:
        r = Row('C002', 'resolve'); r.help(); r.invalid(hint='Usage|usage|invalid')
        d = r.json_ok(args.peer)
        listed = {m['alias']: m['id'] for m in Row('C002', 'machine list').json_ok().get('machines', [])}
        r.expect(isinstance(d, dict) and d.get('machine_id') == listed.get(args.peer) and d.get('alias') == args.peer,
                 'resolve --json does not name the requested machine')
        r.expect(isinstance(d, dict) and re.fullmatch(r'127\.100\.\d+\.\d+', d.get('assigned_ip', '')) is not None,
                 'resolve --json lacks a 127.100.x.x assigned address')
        r.expect(isinstance(d, dict) and isinstance(d.get('forwarded_ports', []), list), 'resolve --json forwarded_ports is not a list')
        r.expect(isinstance(d, dict) and 'tags' not in d, 'resolve still reports tags')
        human = run('resolve', args.peer)[1]
        r.expect('Machine:' in human and 'IP:' in human and (d or {}).get('assigned_ip', 'x') in human, 'resolve human output lacks machine and IP')
        by_id = json.loads(run('resolve', listed.get(args.peer, 'x'), '--json')[1] or '{}')
        r.expect(by_id == d, 'resolve by ID differs from resolve by alias')
        code, o, e = run('resolve', 'no-such-machine-zz')
        r.expect(code != 0 and 'No machine has that name or ID' in o + e and not BAD.search(o + e), 'unknown machine gave no clear message')
        out.append(r)

        r = Row('C124', 'access'); r.help(); r.invalid('bogus')
        code, o, e = run('access')
        r.expect(code == 0 and 'machine' in o and 'tunnel' in o, 'pb access does not list machine and tunnel')
        out.append(r)

        r = Row('C125', 'access machine'); r.help(); r.invalid(args.peer, hint='Usage|usage|invalid')
        code, o, e = run('access', 'machine', 'no-such-machine-zz', '--port', '80')
        r.expect(code != 0 and 'No machine has that name or ID' in o + e, 'access to an unknown machine gave no clear message')
        code, o, e = run('access', 'machine', args.peer, '--port', '80', '--listen', '10.0.0.1:0')
        r.expect(code != 0 and not BAD.search(o + e), 'non-loopback --listen was accepted')
        out.append(r)

        r = Row('C126', 'access tunnel'); r.help(); r.invalid(hint='Usage|usage|invalid')
        code, o, e = run('access', 'tunnel', 'tunnel_zz_missing', '--listen', '10.0.0.1:0')
        r.expect(code != 0 and not BAD.search(o + e), 'non-loopback --listen was accepted')
        out.append(r)

    for ident, name, schema, key in (('C139', 'edge', 'paperboat.edge-list/v1', 'edges'), ('C141', 'relay', 'paperboat.relay-list/v1', 'relays')):
        r = Row(ident, name); r.help(); r.invalid('bogus', hint='Usage|usage|invalid')
        listing = Row(ident, name + ' list'); d = listing.json_ok(); r.failures += listing.failures
        r.expect(isinstance(d, dict) and d.get('schema') == schema and d.get('pool_mode'), name + ' list --json lacks schema or pool mode')
        items = (d or {}).get(key) or []
        r.expect(len(items) > 0 and all(i.get('region') and i.get('status') and i.get('source') for i in items), name + ' list shows no usable ' + key)
        human = run(name, 'list')[1].splitlines()
        r.expect(len(human) == len(items) + 1 and human[0].split()[0] == name.upper(), name + ' list table does not match its JSON')
        out.append(r)
    return out


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('machine')
    parser.add_argument('--offline-alias')
    parser.add_argument('--peer', help='another machine alias to resolve and access')
    parser.add_argument('--only', default='', help='comma list of row ids')
    parser.add_argument('--sudo-password-stdin', action='store_true')
    args = parser.parse_args()
    args.sudo_password = sys.stdin.readline().rstrip('\n') if args.sudo_password_stdin else None
    failed = 0
    only = set(filter(None, args.only.split(',')))
    selected = access_rows(args) if only and only <= ACCESS_ROWS else (rows(args) + access_rows(args))
    for row in selected:
        if only and row.ident not in only:
            continue
        status = 'PASS' if not row.failures else 'FAIL'
        failed += status == 'FAIL'
        print(json.dumps({'check': 'command-row', 'row': row.ident, 'command': 'pb ' + ' '.join(row.command),
                          'status': status, 'failures': row.failures}), flush=True)
    sys.exit(1 if failed else 0)


if __name__ == '__main__':
    main()
