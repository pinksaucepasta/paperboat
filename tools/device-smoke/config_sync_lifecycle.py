#!/usr/bin/env python3
"""F11 on one Linux, macOS or Windows machine: repositories, credentials, sync, approval, conflicts, force, unassign.

Runs ON the paired machine (copy tools/device-smoke there, `python3 config_sync_lifecycle.py ALIAS`).
Everything it creates lives under ~/.cache/pb-cfg-lifecycle and is removed at the end; the machine's own
config-sync.toml is saved and restored. Three repositories are connected and used: a local bare repository,
an HTTP repository served by a small Git smart-HTTP server on 127.0.0.1 with a token profile, and an SSH
repository on this machine's own sshd with a throwaway key (skipped with a note when sshd is unavailable).

Windows (PB_SMOKE_WINDOWS_PB set): runs on the controller; pb needs no Git on the machine (go-git), so Git stays
here. The local repository is built here and copied file by file to ~/.cache/pb-cfg-lifecycle on the
device (and read back the same way); the HTTP server and the SSH repository (this controller's sshd, the throwaway
key in this user's authorized_keys) are reached by the device through `ssh -R` loopback ports. The config worker
is the PaperboatRuntimeConfig service, stopped and started from the SSH session (which must be elevated).

Checks (one PASS/FAIL line each, JSON summary at the end):
  F11-01 connect + list for local/http/ssh, credentials set (private file), remove (validation says so), set again
  F11-02 validate + apply, repository -> machine and machine -> repository in both directions, status healthy
  F11-03 review_required then `pb config approve`; a same-line conflict is listed ONCE, shown, compared,
         resolved both ways (`--keep repository`, `--keep machine`) and the other side shows the same bytes
  F11-04 force pull and force push, unassign (state disabled, no conflict left, worker removed), team default
         refusal for a custom repository
"""
import base64
import contextlib
import getpass
import hashlib
import json
import os
import platform
import shlex
import shutil
import stat
import subprocess
import sys
import tempfile
import time
import traceback
import urllib.request
import glob
from pathlib import Path

import winhost

HOME = Path.home()
ROOT = HOME / '.cache' / 'pb-cfg-lifecycle'
CONFIG_DIR = HOME / '.config' / 'paperboat'
MACHINE_FILE = CONFIG_DIR / 'config-sync.toml'
RUNTIME_ROOT = HOME / '.local/state/paperboat/runtime'
UNIT = 'paperboat-runtime-config.service'
DARWIN = platform.system() == 'Darwin'
LAUNCHD_LABEL = 'com.pinksaucepasta.paperboat.runtime-config'  # cli/internal/hostruntime/service ConfigLabel, a LaunchAgent
WINDOWS_SERVICE = 'PaperboatRuntimeConfig*'  # service.windowsServiceName(ConfigKind, <instance of the user's SID>)
DEVICE = winhost.device()  # the Windows device when PB_SMOKE_WINDOWS_PB is set; pb and the machine's files are there
PB = winhost.pb_path() if DEVICE else 'pb'  # pb from PATH on the machine itself
MIRRORS = {}  # bare repository here -> its copy on the Windows device
SYNC_WAIT = 240
results = {}


def pb(*args, stdin=None, timeout=180):
    return winhost.pb_text(PB, list(args), input=stdin, timeout=timeout)


def use_platform_paths():
    """macOS and Windows keep pb's config, machine file and runtime state elsewhere; ask pb for them. Linux keeps the defaults above."""
    global CONFIG_DIR, MACHINE_FILE, RUNTIME_ROOT
    if DEVICE:
        MACHINE_FILE = winhost.RemotePath(DEVICE, pb('config', 'sync', 'path').stdout.strip())
        RUNTIME_ROOT = DEVICE.runtime_dir()
        return
    if not DARWIN:
        return
    os.environ['PATH'] = str(HOME / '.local/bin') + os.pathsep + os.environ['PATH']
    CONFIG_DIR = Path(pb('config', 'path').stdout.strip()).parent
    MACHINE_FILE = Path(pb('config', 'sync', 'path').stdout.strip())
    RUNTIME_ROOT = HOME / 'Library/Application Support/Paperboat/runtime'


def pb_json(*args, stdin=None, ok=True):
    result = pb(*args, '--json', stdin=stdin)
    try:
        value = json.loads(result.stdout)
    except ValueError:
        raise RuntimeError('not JSON: pb %s -> %s %s' % (' '.join(args), result.stdout[-200:], result.stderr[-200:]))
    if isinstance(value, dict) and 'ok' in value and 'schema_version' in value:
        if ok and value['ok'] is False:
            raise RuntimeError('pb %s failed: %s' % (' '.join(args), value.get('error', {}).get('message')))
        return value.get('data', value)
    return value


def confirmed(*args):
    preview = pb_json(*args, ok=False)
    token = preview.get('confirmation_token')
    # A refusal (unreachable, forbidden, state) is not a preview: say which, so the cause is in the failure.
    assert token, 'no confirmation preview for %s: %s' % (' '.join(args), {k: preview.get(k) for k in ('outcome', 'code', 'message', 'error') if preview.get(k)})
    return pb_json(*args, '--confirm', token)


def wait(what, check, seconds=SYNC_WAIT, every=4):
    end = time.time() + seconds
    last = None
    while time.time() < end:
        last = check()
        if last:
            return last
        time.sleep(every)
    raise AssertionError('timed out waiting for ' + what)


def read(path):
    try:
        return path.read_text() if DEVICE else Path(path).read_text()
    except OSError:
        return None


def private(path):
    """Owner-only: mode 0600, or on Windows the ACL pb gives its own private files."""
    if DEVICE:
        path.private()
    else:
        path.chmod(0o600)


def tree(bare):
    return {path.relative_to(bare).as_posix(): hashlib.sha256(path.read_bytes()).hexdigest()
            for path in bare.rglob('*') if path.is_file() and path.parent.name != 'hooks'}


def to_device(bare):
    """Copy what the device's copy of this bare repository lacks; objects before the references that name them.

    shortcut: overwrites the device's branch like a forced push, fine while the worker is idle or stopped as it is at
    every edit here; upgrade to a compare of the device's reference if an edit ever races a publish."""
    theirs = MIRRORS[bare].tree_hashes() if MIRRORS[bare].exists() else {}
    changed = [name for name, digest in tree(bare).items() if theirs.get(name) != digest]
    for name in sorted(changed, key=lambda name: (name.startswith('refs/'), name)):
        (MIRRORS[bare] / name).write_bytes((bare / name).read_bytes())


def from_device(bare):
    """Fetch what pb on the device wrote to its copy (a publish: objects, then the branch)."""
    mine = tree(bare)
    for name, digest in sorted(MIRRORS[bare].tree_hashes().items(), key=lambda item: (item[0].startswith('refs/'), item[0])):
        if mine.get(name) != digest:
            (bare / name).parent.mkdir(parents=True, exist_ok=True)
            (bare / name).write_bytes((MIRRORS[bare] / name).read_bytes())


def on_device(bare, root):
    """The Windows device has no Git: this bare repository is mirrored to root on the device; returns that path."""
    MIRRORS[bare] = root / bare.name
    to_device(bare)
    return MIRRORS[bare]


def check(name, action):
    started = time.time()
    try:
        detail = action()
        results[name] = 'PASS'
        print('PASS %s %.0fs %s' % (name, time.time() - started, detail or ''), flush=True)
    except Exception as error:  # noqa: BLE001 one failing check must not hide the others
        results[name] = 'FAIL'
        # The type and the failing line: an assert or KeyError alone prints nothing useful.
        frame = traceback.extract_tb(error.__traceback__)[-1]
        print('FAIL %s %.0fs %s: %s (line %d: %s)' % (name, time.time() - started, type(error).__name__,
                                                   str(error)[:300], frame.lineno, frame.line), flush=True)


def git(*args, cwd=None):
    environment = dict(os.environ, GIT_AUTHOR_NAME='pb', GIT_AUTHOR_EMAIL='pb@example.test',
                       GIT_COMMITTER_NAME='pb', GIT_COMMITTER_EMAIL='pb@example.test')
    subprocess.run(['git', *args], check=True, capture_output=True, cwd=cwd, env=environment)


def new_bare(name, content):
    bare = ROOT / (name + '.git')
    git('init', '-q', '--bare', '-b', 'main', str(bare))
    git('-C', str(bare), 'config', 'http.receivepack', 'true')
    edit_repository(bare, content)
    return bare


def edit_repository(bare, content):
    """Commit `content` as the file `cfg/value.txt` through a scratch clone."""
    if bare in MIRRORS:
        from_device(bare)
    work = Path(tempfile.mkdtemp(dir=ROOT))
    try:
        subprocess.run(['git', 'clone', '-q', str(bare), str(work)], check=True, capture_output=True)
        git('checkout', '-q', '-B', 'main', cwd=work)
        (work / 'cfg').mkdir(exist_ok=True)
        (work / 'cfg' / 'value.txt').write_text(content)
        git('add', '-A', cwd=work)
        git('commit', '-q', '--allow-empty', '-m', 'edit', cwd=work)
        git('push', '-q', 'origin', 'main', cwd=work)
    finally:
        shutil.rmtree(work, ignore_errors=True)
    if bare in MIRRORS:
        to_device(bare)


def repository_value(bare):
    if bare in MIRRORS:
        from_device(bare)
    result = subprocess.run(['git', '--git-dir', str(bare), 'show', 'main:cfg/value.txt'], capture_output=True, text=True)
    return result.stdout if result.returncode == 0 else None


HTTP_SERVER = r'''
import base64, http.server, os, subprocess, sys
ROOT, USER, PASS = sys.argv[1], sys.argv[2], sys.argv[3]
EXPECT = 'Basic ' + base64.b64encode((USER + ':' + PASS).encode()).decode()
class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_): pass
    def handle_any(self):
        if self.headers.get('Authorization') != EXPECT:
            self.send_response(401); self.send_header('WWW-Authenticate', 'Basic realm="git"'); self.end_headers(); return
        path, _, query = self.path.partition('?')
        env = dict(os.environ, GIT_PROJECT_ROOT=ROOT, GIT_HTTP_EXPORT_ALL='1', REQUEST_METHOD=self.command, PATH_INFO=path,
                   QUERY_STRING=query, CONTENT_TYPE=self.headers.get('Content-Type', ''), REMOTE_USER=USER, REMOTE_ADDR='127.0.0.1',
                   CONTENT_LENGTH=self.headers.get('Content-Length', ''))
        if self.headers.get('Content-Encoding'): env['HTTP_CONTENT_ENCODING'] = self.headers['Content-Encoding']
        body = b''
        if self.headers.get('Transfer-Encoding') == 'chunked':
            while True:
                size = int(self.rfile.readline().strip(), 16)
                if size == 0: self.rfile.readline(); break
                body += self.rfile.read(size); self.rfile.readline()
            env['CONTENT_LENGTH'] = str(len(body))
        elif self.headers.get('Content-Length'):
            body = self.rfile.read(int(self.headers['Content-Length']))
        out = subprocess.run(['git', 'http-backend'], input=body, env=env, capture_output=True).stdout
        head, _, payload = out.partition(b'\r\n\r\n')
        status, headers = 200, []
        for line in head.decode().split('\r\n'):
            key, _, value = line.partition(': ')
            if key.lower() == 'status': status = int(value.split()[0])
            elif key: headers.append((key, value))
        self.send_response(status)
        for key, value in headers: self.send_header(key, value)
        self.send_header('Content-Length', str(len(payload))); self.end_headers(); self.wfile.write(payload)
    do_GET = do_POST = handle_any
server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
print(server.server_address[1], flush=True)
server.serve_forever()
'''


class Fixture:
    """Everything outside the product that the checks need, with its cleanup."""

    def __init__(self):
        self.processes = []
        self.forwards = contextlib.ExitStack()
        self.authorized_marker = None
        self.password = base64.b32encode(os.urandom(15)).decode().lower()

    def start_http(self, bare_parent):
        process = subprocess.Popen(['python3', '-c', HTTP_SERVER, str(bare_parent), 'cfguser', self.password],
                                   stdout=subprocess.PIPE, text=True, start_new_session=True)
        self.processes.append(process)
        return self.reach(int(process.stdout.readline()))

    def reach(self, port):
        """The loopback port at which the machine reaches this host's 127.0.0.1:port: the same, or an ssh -R forward."""
        return self.forwards.enter_context(DEVICE.forward(port)) if DEVICE else port

    def enable_ssh(self, machine_root):
        """A throwaway key for this host's sshd; returns the key, known_hosts and host[:port] as the machine uses them."""
        key = ROOT / 'ssh_key'
        subprocess.run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', str(key), '-C', 'pb-cfg-lifecycle'], check=True)
        authorized = HOME / '.ssh' / 'authorized_keys'
        authorized.parent.mkdir(mode=0o700, exist_ok=True)
        with authorized.open('a') as handle:
            handle.write(Path(str(key) + '.pub').read_text())
        self.authorized_marker = 'pb-cfg-lifecycle'
        scanned = subprocess.run(['ssh-keyscan', '-t', 'ed25519', '127.0.0.1'], capture_output=True, text=True).stdout
        if not scanned.strip():
            raise RuntimeError('sshd on 127.0.0.1 is not available')
        (ROOT / 'known_hosts').write_text(scanned)
        (ROOT / 'known_hosts').chmod(0o600)  # the runtime refuses a credential file others can write
        probe = subprocess.run(['ssh', '-i', str(key), '-o', 'BatchMode=yes', '-o', 'UserKnownHostsFile=' + str(ROOT / 'known_hosts'),
                                '-o', 'IdentitiesOnly=yes', getpass.getuser() + '@127.0.0.1', 'git --version'], capture_output=True, text=True, timeout=30)
        if probe.returncode != 0:
            raise RuntimeError('ssh to this machine failed: ' + probe.stderr.strip()[-150:])
        if not DEVICE:
            return key, ROOT / 'known_hosts', '127.0.0.1'
        port = self.reach(22)  # the port ssh-keyscan and the probe above used
        for name, text in (('ssh_key', key.read_text()), ('known_hosts', scanned.replace('127.0.0.1 ', '[127.0.0.1]:%d ' % port))):
            (machine_root / name).write_text(text)
            (machine_root / name).private()  # the runtime refuses a credential file with any other ACL
        return machine_root / 'ssh_key', machine_root / 'known_hosts', '127.0.0.1:%d' % port

    def close(self):
        self.forwards.close()
        for process in self.processes:
            try:
                os.killpg(process.pid, 15)
            except OSError:
                pass
        if self.authorized_marker:
            authorized = HOME / '.ssh' / 'authorized_keys'
            lines = [line for line in authorized.read_text().splitlines(True) if self.authorized_marker not in line]
            authorized.write_text(''.join(lines))


def api_token_request(path, method='GET'):
    secrets = glob.glob(str(CONFIG_DIR / 'credentials/secrets/profile-*-access.secret'))
    if DEVICE:  # DPAPI files, and no config.json until a setting changes
        from bugreport_lifecycle import access_token
        token, server = access_token(), pb_json('config', 'show')['server_url']
    elif secrets:
        token = Path(secrets[0]).read_text().strip()
    else:  # macOS keeps the token in the login keychain
        from bugreport_lifecycle import keychain_tokens
        token = next(v for k, v in keychain_tokens().items() if k.startswith('keychain-access'))
    if not DEVICE:
        server = json.loads((CONFIG_DIR / 'config.json').read_text())['server_url']
    request = urllib.request.Request(server + path, method=method, headers={'Authorization': 'Bearer ' + token, 'X-Paperboat-Workspace': 'personal'})
    with urllib.request.urlopen(request, timeout=30) as response:
        return response.status


def write_machine_file(repository_id, local, automatic='true', extra=''):
    MACHINE_FILE.parent.mkdir(parents=True, exist_ok=True)
    MACHINE_FILE.write_text('version = 1\nenabled = true\nautomatic_updates = %s\nmode = "bidirectional"\n%s'
                            '[pull]\nrepository_id = "%s"\n[push]\nrepository_id = "%s"\n'
                            '[paths."cfg/value.txt"]\nlocal_path = "%s"\nkind = "file"\n'
                            # a Windows path: its backslashes are escapes in a TOML basic string
                            % (automatic, extra, repository_id, repository_id, str(local).replace('\\', '\\\\')))
    private(MACHINE_FILE)


def apply_configuration():
    preview = pb_json('config', 'sync', 'apply', ok=False)
    return pb_json('config', 'sync', 'apply', '--confirm', preview['confirmation_token'])


def my_status(alias):
    status = pb_json('config', 'status')
    return next(e for e in status['environments'] if e['alias'] == alias)


def systemctl(*args):
    if DEVICE:  # stop/start of the config worker's Windows service; a failure raises with the step
        DEVICE.service(args[0], WINDOWS_SERVICE)
        return subprocess.CompletedProcess(args, 0, '', '')
    if DARWIN:
        return launchctl(*args)
    return subprocess.run(['systemctl', '--user', *args], capture_output=True, text=True)


def launchctl(action, unit):
    """The systemctl verbs this script uses, on the config worker LaunchAgent (KeepAlive, so stop is a bootout)."""
    domain = 'gui/%d' % os.getuid()
    target = domain + '/' + LAUNCHD_LABEL

    def run(*words):
        return subprocess.run(['launchctl', *words], capture_output=True, text=True)
    if action == 'stop':
        result = run('bootout', target)
        for _ in range(50):  # bootout returns before launchd has reaped the job
            if run('print', target).returncode != 0:
                break
            time.sleep(0.2)
        return result
    if action == 'start':
        for _ in range(25):
            result = run('bootstrap', domain, str(HOME / 'Library/LaunchAgents' / (LAUNCHD_LABEL + '.plist')))
            if result.returncode == 0:
                break
            time.sleep(0.4)
        return result
    raise ValueError('unsupported verb ' + action)


def credentials_private():
    directory = RUNTIME_ROOT / 'config-sync/repository-credentials'
    if DEVICE:  # nobody but the user, SYSTEM and Administrators in the ACL: what pb calls private on Windows
        names = DEVICE.files(directory)
        return bool(names) and not any(DEVICE.other_access(directory / name) for name in names)
    files = glob.glob(str(directory / '*'))
    return files and all(stat.S_IMODE(os.stat(f).st_mode) == 0o600 for f in files)


def worker_gone():
    if DEVICE:
        return not [s for s in DEVICE.services(WINDOWS_SERVICE) if s['Status'] == 'Running']
    if DARWIN:
        return subprocess.run(['launchctl', 'print', 'gui/%d/%s' % (os.getuid(), LAUNCHD_LABEL)], capture_output=True).returncode != 0
    return systemctl('is-enabled', UNIT).returncode != 0 or systemctl('is-active', UNIT).stdout.strip() != 'active'


def main():
    alias = sys.argv[1]
    use_platform_paths()
    shutil.rmtree(ROOT, ignore_errors=True)  # a killed earlier run leaves its key and repositories
    ROOT.mkdir(parents=True)
    backup = MACHINE_FILE.read_text() if MACHINE_FILE.exists() else None
    fixture = Fixture()
    repositories = {}
    machine_root = ROOT  # where the machine's own files of this check live
    if DEVICE:
        machine_root = DEVICE.home() / '.cache' / 'pb-cfg-lifecycle'
        machine_root.unlink()
        machine_root.mkdir()
    managed = machine_root / 'managed.txt'
    try:
        # ---- F11-01: connect, list, credentials ----
        local_bare = new_bare('local', 'seed local\n')
        local_url = str(on_device(local_bare, machine_root) if DEVICE else local_bare)
        http_port = fixture.start_http(ROOT)
        http_bare = new_bare('http', 'seed http\n')
        http_url = 'http://127.0.0.1:%d/http.git' % http_port
        profile = json.dumps({'transport': 'http', 'auth': 'token', 'username': 'cfguser', 'password': fixture.password, 'allow_insecure_http': True})
        ssh_error = None
        try:
            ssh_key, known_hosts, ssh_host = fixture.enable_ssh(machine_root)
            ssh_bare = new_bare('ssh', 'seed ssh\n')
            ssh_url = 'ssh://%s@%s%s' % (getpass.getuser(), ssh_host, ssh_bare)
        except Exception as error:  # noqa: BLE001
            ssh_error = str(error)

        def connect_all():
            for name, url in (('local', local_url), ('http', http_url)) + ((('ssh', ssh_url),) if ssh_error is None else ()):
                data = pb_json('config', 'repository', 'connect', url, '--name', 'lifecycle-' + name)
                repositories[name] = data['repository']['id']
                assert data['repository']['external_ref'] == url, data
            listed = pb_json('config', 'repository', 'list')['repositories']
            ids = {r['id'] for r in listed}
            assert set(repositories.values()) <= ids, 'connected repositories missing from the list'
            return ', '.join(sorted(repositories))
        check('F11-01 connect+list local/http/ssh', connect_all)

        def credentials():
            saved = pb_json('config', 'repository', 'credentials', 'set', repositories['http'], '--file', '-', stdin=profile)
            assert saved['state'] == 'saved', saved
            assert credentials_private(), 'credential profile files are not owner-only'
            assert fixture.password not in json.dumps(saved) and fixture.password not in pb('config', 'repository', 'list').stdout
            if ssh_error is None:
                ssh_profile = json.dumps({'transport': 'ssh', 'auth': 'ssh', 'ssh_key_file': str(ssh_key), 'known_hosts_file': str(known_hosts), 'username': getpass.getuser()})
                assert pb_json('config', 'repository', 'credentials', 'set', repositories['ssh'], '--file', '-', stdin=ssh_profile)['state'] == 'saved'
            write_machine_file(repositories['http'], managed)
            assert pb_json('config', 'sync', 'validate')['pull_repository_id'] == repositories['http']
            removed = pb_json('config', 'repository', 'credentials', 'remove', repositories['http'])
            assert removed, removed
            refused = pb('config', 'sync', 'validate', '--json')
            assert 'credentials' in refused.stdout.lower(), 'validate without credentials did not name the credentials: ' + refused.stdout[:200]
            pb_json('config', 'repository', 'credentials', 'set', repositories['http'], '--file', '-', stdin=profile)
            assert pb_json('config', 'sync', 'validate')['pull_repository_id'] == repositories['http']
            return 'set, private, removed -> named, set again'
        check('F11-01 credentials set/remove (http)', credentials)
        if ssh_error is None:
            def ssh_validate():
                write_machine_file(repositories['ssh'], managed)
                assert pb_json('config', 'sync', 'validate')['pull_repository_id'] == repositories['ssh']
            check('F11-01 ssh repository validates with a key profile', ssh_validate)
        else:
            results['F11-01 ssh repository validates with a key profile'] = 'SKIP'
            print('SKIP F11-01 ssh: ' + ssh_error)

        # ---- F11-02 and F11-03 on the local repository, then the http one ----
        def assign(name, bare):
            managed.unlink(missing_ok=True)
            write_machine_file(repositories[name], managed)
            projection = pb_json('config', 'sync', 'validate')
            assert projection['pull_repository_id'] == repositories[name] and projection['mode'] == 'bidirectional'
            apply_configuration()
            wait('seed pulled to the machine (%s)' % name, lambda: read(managed) == repository_value(bare))
            wait('status healthy', lambda: my_status(alias)['state'] == 'healthy')

        def both_ways(name, bare):
            assign(name, bare)
            edit_repository(bare, 'from repository %s\n' % name)
            wait('repository edit applied', lambda: read(managed) == 'from repository %s\n' % name)
            managed.write_text('from machine %s\n' % name)
            wait('machine edit published', lambda: repository_value(bare) == 'from machine %s\n' % name)
            return 'repo->machine and machine->repo'
        check('F11-02 assign/apply/sync both ways (local repository)', lambda: both_ways('local', local_bare))
        check('F11-02 assign/apply/sync both ways (http repository + credentials)', lambda: both_ways('http', http_bare))

        # approval flow: automatic updates off
        def approval():
            managed.unlink(missing_ok=True)
            write_machine_file(repositories['local'], managed, automatic='false')
            apply_configuration()
            wait('first review', lambda: my_status(alias).get('error_code') == 'review_required')
            assert not managed.exists(), 'applied before approval'
            pb_json('config', 'approve', alias)
            wait('first review applied', lambda: read(managed) == repository_value(local_bare))
            wait('status healthy', lambda: my_status(alias)['state'] == 'healthy')
            edit_repository(local_bare, 'needs approval\n')
            wait('review required', lambda: my_status(alias).get('error_code') == 'review_required')
            assert read(managed) != 'needs approval\n', 'applied before approval'
            pb_json('config', 'approve', alias)
            wait('approved revision applied', lambda: read(managed) == 'needs approval\n')
            return 'review_required -> approve -> applied (initial and later revision)'
        check('F11-03 review required then approve', approval)
        write_machine_file(repositories['local'], managed)
        apply_configuration()

        def mine():
            return [c for c in pb_json('config', 'conflict', 'list')['conflicts'] if c['environment_name'] == alias]

        def make_conflict(machine_text, repository_text):
            systemctl('stop', UNIT)
            managed.write_text(machine_text)
            edit_repository(local_bare, repository_text)
            systemctl('start', UNIT)
            return wait('one listed conflict', lambda: len(mine()) >= 1)

        def conflict_flow(keep, expected_machine, expected_repository, winner):
            make_conflict(expected_machine, expected_repository)
            listed = mine()
            assert len(listed) == 1, 'conflict listed %d times' % len(listed)
            path = listed[0]['conflict']['path']
            shown = pb_json('config', 'conflict', 'show', alias, path)
            assert shown['conflict']['path'] == path and shown['conflict']['revision'] == listed[0]['conflict']['revision']
            compare = None
            for _ in range(4):
                result = pb('config', 'conflict', 'compare', alias, path, '--json')
                try:
                    value = json.loads(result.stdout)
                except ValueError:
                    value = {}
                if value.get('local', {}).get('present') is not None and 'ok' not in value:
                    compare = value
                    break
                time.sleep(3)
            assert compare is not None, 'compare never returned both versions: ' + (result.stdout or result.stderr)[-300:]
            assert base64.b64decode(compare['local']['content']).decode() == expected_machine, compare['local']
            assert base64.b64decode(compare['managed']['content']).decode() == expected_repository, compare['managed']
            resolved = pb_json('config', 'conflict', 'resolve', alias, path, '--keep', keep)
            assert resolved['outcome'] == 'queued', resolved
            wait('conflict resolved', lambda: not mine())
            wait('both sides carry the winner', lambda: read(managed) == winner and repository_value(local_bare) == winner)
            return 'listed once, shown, compared, resolved --keep ' + keep
        check('F11-03 conflict: list/show/compare/resolve --keep repository',
              lambda: conflict_flow('repository', 'machine A\n', 'repository A\n', 'repository A\n'))
        check('F11-03 conflict: list/show/compare/resolve --keep machine',
              lambda: conflict_flow('machine', 'machine B\n', 'repository B\n', 'machine B\n'))

        # ---- F11-04: force, unassign, team default ----
        def force_pull():
            edit_repository(local_bare, 'force pull source\n')
            managed.write_text('machine side before force pull\n')
            confirmed('config', 'force', 'pull', alias)
            wait('force pull applied', lambda: read(managed) == 'force pull source\n')
            return 'repository won'
        check('F11-04 force pull', force_pull)

        def force_push():
            managed.write_text('force push source\n')
            edit_repository(local_bare, 'repository side before force push\n')
            confirmed('config', 'force', 'push', alias)
            wait('force push published', lambda: repository_value(local_bare) == 'force push source\n')
            return 'machine won'
        check('F11-04 force push', force_push)

        def unassign():
            confirmed('config', 'unassign', alias)
            assert my_status(alias)['state'] == 'disabled'
            assert not [c for c in pb_json('config', 'conflict', 'list')['conflicts'] if c['environment_name'] == alias], 'conflict left behind'
            wait('worker removed', worker_gone, 60, 3)
            return 'disabled, no conflict, worker gone'
        check('F11-04 unassign', unassign)

        def team_default():
            teams = pb_json('team', 'list')
            if not teams:
                raise AssertionError('this account has no team')
            refused = pb('config', 'team-default-set', teams[0]['name'], repositories['local'], '--json')
            assert 'GitHub repository' in refused.stdout, refused.stdout[:200]
            return 'custom repository refused with the reason; adopt/unadopt need a GitHub repository'
        check('F11-04 team default refusal for a custom repository', team_default)
    finally:
        try:
            pb('config', 'unassign', alias, '--json', '--confirm', pb_json('config', 'unassign', alias, ok=False).get('confirmation_token', 'x'))
        except Exception:  # noqa: BLE001
            pass
        fixture.close()
        if backup is None:
            MACHINE_FILE.unlink(missing_ok=True)
        else:
            MACHINE_FILE.write_text(backup)
            private(MACHINE_FILE)
        for repository_id in repositories.values():
            try:
                api_token_request('/v1/config-repositories/' + repository_id, 'DELETE')
            except Exception:  # noqa: BLE001
                pass
        shutil.rmtree(ROOT, ignore_errors=True)
        if DEVICE:
            machine_root.unlink()
    failed = [name for name, outcome in results.items() if outcome == 'FAIL']
    print(json.dumps({'check': 'config-sync-lifecycle', 'status': 'FAIL' if failed else 'PASS', 'results': results}), flush=True)
    sys.exit(1 if failed else 0)


if __name__ == '__main__':
    main()
