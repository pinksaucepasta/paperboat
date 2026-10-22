#!/usr/bin/env python3
"""F11 on one Linux machine: repositories, credentials, sync, approval, conflicts, force, unassign.

Runs ON the paired machine (copy tools/device-smoke there, `python3 config_sync_lifecycle.py ALIAS`).
Everything it creates lives under ~/.cache/pb-cfg-lifecycle and is removed at the end; the machine's own
config-sync.toml is saved and restored. Three repositories are connected and used: a local bare repository,
an HTTP repository served by a small Git smart-HTTP server on 127.0.0.1 with a token profile, and an SSH
repository on this machine's own sshd with a throwaway key (skipped with a note when sshd is unavailable).

Checks (one PASS/FAIL line each, JSON summary at the end):
  F11-01 connect + list for local/http/ssh, credentials set (private file), remove (validation says so), set again
  F11-02 validate + apply, repository -> machine and machine -> repository in both directions, status healthy
  F11-03 review_required then `pb config approve`; a same-line conflict is listed ONCE, shown, compared,
         resolved both ways (`--keep repository`, `--keep machine`) and the other side shows the same bytes
  F11-04 force pull and force push, unassign (state disabled, no conflict left, worker removed), team default
         refusal for a custom repository
"""
import base64
import getpass
import json
import os
import shlex
import shutil
import stat
import subprocess
import sys
import tempfile
import time
import urllib.request
import glob
from pathlib import Path

HOME = Path.home()
ROOT = HOME / '.cache' / 'pb-cfg-lifecycle'
MACHINE_FILE = HOME / '.config' / 'paperboat' / 'config-sync.toml'
UNIT = 'paperboat-runtime-config.service'
SYNC_WAIT = 240
results = {}


def pb(*args, stdin=None, timeout=180):
    return subprocess.run(['pb', *args], capture_output=True, text=True, timeout=timeout, input=stdin)


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
    assert token, 'no confirmation preview for ' + ' '.join(args)
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
        return Path(path).read_text()
    except OSError:
        return None


def check(name, action):
    started = time.time()
    try:
        detail = action()
        results[name] = 'PASS'
        print('PASS %s %.0fs %s' % (name, time.time() - started, detail or ''), flush=True)
    except Exception as error:  # noqa: BLE001 one failing check must not hide the others
        results[name] = 'FAIL'
        print('FAIL %s %.0fs %s' % (name, time.time() - started, str(error)[:300]), flush=True)


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


def repository_value(bare):
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
        self.authorized_marker = None
        self.password = base64.b32encode(os.urandom(15)).decode().lower()

    def start_http(self, bare_parent):
        process = subprocess.Popen(['python3', '-c', HTTP_SERVER, str(bare_parent), 'cfguser', self.password],
                                   stdout=subprocess.PIPE, text=True, start_new_session=True)
        self.processes.append(process)
        return int(process.stdout.readline())

    def enable_ssh(self):
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
        return key, ROOT / 'known_hosts'

    def close(self):
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
    secrets = glob.glob(str(HOME / '.config/paperboat/credentials/secrets/profile-*-access.secret'))
    token = Path(secrets[0]).read_text().strip()
    server = json.loads((HOME / '.config/paperboat/config.json').read_text())['server_url']
    request = urllib.request.Request(server + path, method=method, headers={'Authorization': 'Bearer ' + token, 'X-Paperboat-Workspace': 'personal'})
    with urllib.request.urlopen(request, timeout=30) as response:
        return response.status


def write_machine_file(repository_id, local, automatic='true', extra=''):
    MACHINE_FILE.parent.mkdir(parents=True, exist_ok=True)
    MACHINE_FILE.write_text('version = 1\nenabled = true\nautomatic_updates = %s\nmode = "bidirectional"\n%s'
                            '[pull]\nrepository_id = "%s"\n[push]\nrepository_id = "%s"\n'
                            '[paths."cfg/value.txt"]\nlocal_path = "%s"\nkind = "file"\n' % (automatic, extra, repository_id, repository_id, local))
    MACHINE_FILE.chmod(0o600)


def apply_configuration():
    preview = pb_json('config', 'sync', 'apply', ok=False)
    return pb_json('config', 'sync', 'apply', '--confirm', preview['confirmation_token'])


def my_status(alias):
    status = pb_json('config', 'status')
    return next(e for e in status['environments'] if e['alias'] == alias)


def systemctl(*args):
    return subprocess.run(['systemctl', '--user', *args], capture_output=True, text=True)


def main():
    alias = sys.argv[1]
    shutil.rmtree(ROOT, ignore_errors=True)  # a killed earlier run leaves its key and repositories
    ROOT.mkdir(parents=True)
    backup = MACHINE_FILE.read_text() if MACHINE_FILE.exists() else None
    fixture = Fixture()
    repositories = {}
    managed = ROOT / 'managed.txt'
    try:
        # ---- F11-01: connect, list, credentials ----
        local_bare = new_bare('local', 'seed local\n')
        http_port = fixture.start_http(ROOT)
        http_bare = new_bare('http', 'seed http\n')
        http_url = 'http://127.0.0.1:%d/http.git' % http_port
        profile = json.dumps({'transport': 'http', 'auth': 'token', 'username': 'cfguser', 'password': fixture.password, 'allow_insecure_http': True})
        ssh_error = None
        try:
            ssh_key, known_hosts = fixture.enable_ssh()
            ssh_bare = new_bare('ssh', 'seed ssh\n')
            ssh_url = 'ssh://%s@127.0.0.1%s' % (getpass.getuser(), ssh_bare)
        except Exception as error:  # noqa: BLE001
            ssh_error = str(error)

        def connect_all():
            for name, url in (('local', str(local_bare)), ('http', http_url)) + ((('ssh', ssh_url),) if ssh_error is None else ()):
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
            files = glob.glob(str(HOME / '.local/state/paperboat/runtime/config-sync/repository-credentials/*'))
            assert files and all(stat.S_IMODE(os.stat(f).st_mode) == 0o600 for f in files), 'credential profile files are not owner-only'
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
            assert compare is not None, 'compare never returned both versions'
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
            wait('worker removed', lambda: systemctl('is-enabled', UNIT).returncode != 0 or systemctl('is-active', UNIT).stdout.strip() != 'active', 60, 3)
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
            MACHINE_FILE.chmod(0o600)
        for repository_id in repositories.values():
            try:
                api_token_request('/v1/config-repositories/' + repository_id, 'DELETE')
            except Exception:  # noqa: BLE001
                pass
        shutil.rmtree(ROOT, ignore_errors=True)
    failed = [name for name, outcome in results.items() if outcome == 'FAIL']
    print(json.dumps({'check': 'config-sync-lifecycle', 'status': 'FAIL' if failed else 'PASS', 'results': results}), flush=True)
    sys.exit(1 if failed else 0)


if __name__ == '__main__':
    main()
