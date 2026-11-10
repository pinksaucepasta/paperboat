#!/usr/bin/env python3
"""F11 across two paired Linux, macOS or Windows machines that share one config repository.

Runs on the controller:
  config_sync_pair.py REPO_SHELL REPO_ADDR SHELL_A ALIAS_A SHELL_B ALIAS_B [--machine-sections] [--repo-from-controller=ADDR]
REPO_SHELL runs a shell on the host that serves the repository ("ssh pbvm3"); REPO_ADDR is that host's
address as both machines reach it (172.17.0.1 for containers on it). SHELL_X runs a shell on machine X
("ssh pbvm3 sudo -n docker exec -i -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 lanel-a").
On a Mac use its own ssh ("ssh admin@mac"); the pb machine file, config and os.<platform> section are found per machine.
SHELL_X `windows` is the Windows device (PB_SMOKE_WINDOWS_PB, winhost): only pb.exe and PowerShell snippets run
there. It reaches the repository through an ssh -R loopback port to ADDR (the repository host as this controller
reaches it, default REPO_ADDR) and binds the one registered repository to that endpoint in its machine file
(`url` under [pull]/[push]); the other machine keeps the registered URL.

A Git smart-HTTP server with a token on the repository host serves one bare repository, so the account
registers it once and both machines use the same URL. Checks:
  pair-connect      the repository connects once and both machines save a private token profile
  pair-assign       both validate their own destination, apply, pull the seed and become healthy
  pair-a-to-b       an edit on A reaches the repository and then B, byte-identical
  pair-b-to-a       the same from B to A
  pair-precedence   (--machine-sections) [machine.<A>] moves A's destination over [os.linux]; B keeps the
                    [os.linux] one; then A's own file wins over its [machine.<A>] section
Everything is removed at the end: assignments, machine files, files, credentials, repository, server.
"""
import base64
import contextlib
import json
import os
import shlex
import subprocess
import sys
import time
from functools import cached_property
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import winhost  # noqa: E402
from config_sync_lifecycle import HTTP_SERVER, api_token_request  # noqa: E402

DIR = '.cache/pb-cfg-pair'
REPO_ROOT = '/tmp/pb-cfg-pair-repo'
NAME = 'pair-config-fixture'
SYNC_WAIT = 240
results = {}

DELETE_REPOSITORY = r'''
import glob, json, sys, urllib.request
from pathlib import Path
home = Path.home()
config = home / ('Library/Application Support/paperboat' if sys.platform == 'darwin' else '.config/paperboat')
token = Path(glob.glob(str(config / 'credentials/secrets/profile-*-access.secret'))[0]).read_text().strip()
server = json.loads((config / 'config.json').read_text())['server_url']
request = urllib.request.Request(server + '/v1/config-repositories/' + sys.argv[1], method='DELETE',
                                 headers={'Authorization': 'Bearer ' + token, 'X-Paperboat-Workspace': 'personal'})
print(urllib.request.urlopen(request, timeout=30).status)
'''

GIT_ENV = 'GIT_AUTHOR_NAME=pair GIT_AUTHOR_EMAIL=pair@example.test GIT_COMMITTER_NAME=pair GIT_COMMITTER_EMAIL=pair@example.test'


class Shell:
    def __init__(self, prefix):
        self.prefix = shlex.split(prefix)

    def sh(self, script, stdin=None, timeout=200, check=False):
        result = subprocess.run(self.prefix + ['bash', '-c', shlex.quote('export PATH=$HOME/.local/bin:$PATH; ' + script)],
                                input=stdin, capture_output=True, text=True, timeout=timeout)
        if check and result.returncode:
            raise RuntimeError('command failed: %s: %s' % (script[:120], result.stderr[-300:]))
        return result


class Machine(Shell):
    own_url = None  # this machine's own endpoint for the shared repository, when it cannot use the registered URL

    def __init__(self, prefix, alias):
        super().__init__(prefix)
        self.alias = alias

    def pb_run(self, args, stdin=None):
        return self.sh('pb ' + ' '.join(shlex.quote(a) for a in args), stdin=stdin)

    def pb(self, *args, stdin=None, ok=True):
        result = self.pb_run([*args, '--json'], stdin=stdin)
        try:
            value = json.loads(result.stdout)
        except ValueError:
            raise RuntimeError('%s: pb %s -> %s %s' % (self.alias, ' '.join(args), result.stdout[-200:], result.stderr[-200:]))
        if isinstance(value, dict) and 'schema_version' in value and 'ok' in value:
            if ok and value['ok'] is False:
                raise RuntimeError('%s: pb %s failed: %s' % (self.alias, ' '.join(args), value.get('error', {}).get('message')))
            return value.get('data', value)
        return value

    def read(self, name):
        result = self.sh('cat "$HOME/%s/%s" 2>/dev/null' % (DIR, name))
        return result.stdout if result.returncode == 0 else None

    def write(self, name, text):
        self.sh('mkdir -p "$HOME/%s" && cat > "$HOME/%s/%s"' % (DIR, DIR, name), stdin=text, check=True)

    @cached_property
    def os(self):
        return 'darwin' if self.sh('uname -s').stdout.strip() == 'Darwin' else 'linux'

    @property
    def sync_file(self):
        """Shell expression for this machine's config-sync.toml."""
        return '"$(pb config sync path)"' if self.os == 'darwin' else '~/.config/paperboat/config-sync.toml'

    def write_sync_file(self, text):
        if self.os == 'darwin':
            script = 'mkdir -p "$(dirname {0})" && cat > {0} && chmod 600 {0}'.format(self.sync_file)
        else:
            script = 'mkdir -p ~/.config/paperboat && cat > ~/.config/paperboat/config-sync.toml && chmod 600 ~/.config/paperboat/config-sync.toml'
        self.sh(script, stdin=text, check=True)

    def outputs(self):
        """What the repository list and the status print (must never hold the credential)."""
        return self.sh('pb config repository list --json; pb config status --json').stdout

    def delete_repository(self, repository_id):
        self.sh('python3 - ' + shlex.quote(repository_id), stdin=DELETE_REPOSITORY)

    def remove_files(self):
        self.sh('rm -f %s; rm -rf "$HOME/%s"' % (self.sync_file, DIR))

    def apply(self):
        preview = self.pb('config', 'sync', 'apply', ok=False)
        return self.pb('config', 'sync', 'apply', '--confirm', preview['confirmation_token'])

    def state(self):
        return next(e for e in self.pb('config', 'status')['environments'] if e['alias'] == self.alias)


class WindowsMachine(Machine):
    """The Windows device: the same steps with pb.exe and file snippets through winhost, no shell on the device."""

    os = 'windows'

    def __init__(self, alias):
        self.alias, self.device = alias, winhost.device()
        if not self.device:
            raise RuntimeError('the side `windows` is the Windows device: set PB_SMOKE_WINDOWS_PB to its pb.exe')
        self.root = self.device.home() / DIR

    def pb_run(self, args, stdin=None):
        return winhost.side_run(winhost.WINDOWS, list(args), input=stdin, timeout=200)

    def read(self, name):
        try:
            return (self.root / name).read_text()
        except FileNotFoundError:
            return None

    def write(self, name, text):
        (self.root / name).write_text(text)

    @cached_property
    def sync_file(self):
        return winhost.RemotePath(self.device, self.pb_run(['config', 'sync', 'path']).stdout.strip())

    def write_sync_file(self, text):
        self.sync_file.write_text(text)
        self.sync_file.private()

    def outputs(self):
        return self.pb_run(['config', 'repository', 'list', '--json']).stdout + self.pb_run(['config', 'status', '--json']).stdout

    def delete_repository(self, repository_id):
        api_token_request('/v1/config-repositories/' + repository_id, 'DELETE')

    def remove_files(self):
        self.sync_file.unlink()
        self.root.unlink()


def machine(prefix, alias):
    return WindowsMachine(alias) if winhost.is_side(prefix) else Machine(prefix, alias)


class Repository(Shell):
    """The bare repository and its HTTP server live on the repository host."""

    def seed(self, files):
        script = ['rm -rf %s && mkdir -p %s && git init -q --bare -b main %s/pair.git' % (REPO_ROOT, REPO_ROOT, REPO_ROOT),
                  'work=$(mktemp -d) && git clone -q %s/pair.git $work && cd $work' % REPO_ROOT]
        for name, text in files.items():
            script.append('mkdir -p "$(dirname %s)" && printf %%s %s > %s' % (shlex.quote(name), shlex.quote(text), shlex.quote(name)))
        script.append('git add -A && %s git commit -q -m seed && git push -q origin HEAD:main && rm -rf $work' % GIT_ENV)
        self.sh(' && '.join(script), check=True)

    def serve(self, password):
        self.sh('cat > %s/server.py' % REPO_ROOT, stdin=HTTP_SERVER.replace("('127.0.0.1', 0)", "('0.0.0.0', 0)"), check=True)
        self.sh('cd %s && (setsid nohup python3 server.py %s cfguser %s > port 2>/dev/null < /dev/null & echo $! > pid)'
                % (REPO_ROOT, REPO_ROOT, shlex.quote(password)), check=True)
        for _ in range(20):
            port = self.sh('cat %s/port' % REPO_ROOT).stdout.strip()
            if port:
                return int(port)
            time.sleep(0.5)
        raise RuntimeError('repository server did not start')

    def value(self, path='cfg/value.txt'):
        result = self.sh('git --git-dir %s/pair.git show main:%s' % (REPO_ROOT, path))
        return result.stdout if result.returncode == 0 else None

    def close(self):
        self.sh('kill $(cat %s/pid) 2>/dev/null; rm -rf %s' % (REPO_ROOT, REPO_ROOT))


def wait(what, check, seconds=SYNC_WAIT, every=4):
    end = time.time() + seconds
    while time.time() < end:
        if check():
            return
        time.sleep(every)
    raise AssertionError('timed out waiting for ' + what)


def check(name, action):
    started = time.time()
    try:
        detail = action()
        results[name] = 'PASS'
        print('PASS %s %ds %s' % (name, time.time() - started, detail or ''), flush=True)
    except Exception as error:  # noqa: BLE001
        results[name] = 'FAIL'
        print('FAIL %s %ds: %s' % (name, time.time() - started, str(error)[:400]), flush=True)
        raise


def shared_file(a_alias, sections, platforms=('linux',)):
    text = 'version = 1\n[paths."cfg/value.txt"]\nkind = "file"\nlocal_path = "~/%s/value.txt"\n' % DIR
    if sections:
        for platform in platforms:
            text += '[os.%s.paths."cfg/value.txt"]\nlocal_path = "~/%s/value-%s.txt"\n' % (platform, DIR, platform)
        text += '[machine.%s.paths."cfg/value.txt"]\nlocal_path = "~/%s/value-%s.txt"\n' % (a_alias, DIR, a_alias)
    return text


def machine_file(repository_id, extra='', url=None):
    target = 'repository_id = "%s"\n' % repository_id + ('url = "%s"\n' % url if url else '')
    return ('version = 1\nenabled = true\nautomatic_updates = true\nmode = "bidirectional"\n'
            '[pull]\n%s[push]\n%s%s' % (target, target, extra))


def run_checks(repo, a, b, url, profile, password, sections, repository):
    a_name = 'value-%s.txt' % a.alias if sections else 'value.txt'
    b_name = 'value-%s.txt' % b.os if sections else 'value.txt'

    def connect():
        for stale in [r for r in a.pb('config', 'repository', 'list')['repositories'] if r['display_name'] == NAME]:
            a.delete_repository(stale['id'])
        repository['id'] = a.pb('config', 'repository', 'connect', url, '--name', NAME)['repository']['id']
        for machine in (a, b):
            saved = machine.pb('config', 'repository', 'credentials', 'set', repository['id'], '--file', '-', stdin=profile)
            assert saved['state'] == 'saved', saved
            assert password not in machine.outputs()
        return repository['id']
    check('pair-connect', connect)

    def assign():
        for machine, name in ((a, a_name), (b, b_name)):
            machine.write_sync_file(machine_file(repository['id'], url=machine.own_url))
            projection = machine.pb('config', 'sync', 'validate')
            paths = [r['local_path'] for r in projection['path_rules']]
            assert paths == ['~/%s/%s' % (DIR, name)], '%s resolved %s' % (machine.alias, paths)
            machine.apply()
        wait('A pulled the seed', lambda: a.read(a_name) == 'seed\n')
        wait('B pulled the seed', lambda: b.read(b_name) == 'seed\n')
        wait('both healthy', lambda: a.state()['state'] == 'healthy' and b.state()['state'] == 'healthy')
        return 'A %s, B %s' % (a_name, b_name)
    check('pair-assign', assign)

    def flow(source, source_name, target, target_name):
        text = 'from %s %d\n' % (source.alias, time.time())
        source.write(source_name, text)
        wait('%s published' % source.alias, lambda: repo.value() == text)
        wait('%s received' % target.alias, lambda: target.read(target_name) == text)
        return 'byte-identical through the repository'
    check('pair-a-to-b', lambda: flow(a, a_name, b, b_name))
    check('pair-b-to-a', lambda: flow(b, b_name, a, a_name))

    if sections:
        def local_wins():
            extra = '[paths."cfg/value.txt"]\nlocal_path = "~/%s/value-own.txt"\n' % DIR
            a.write_sync_file(machine_file(repository['id'], extra, a.own_url))
            rules = [(r['local_path'], r['source']) for r in a.pb('config', 'sync', 'validate')['path_rules']]
            assert rules == [('~/%s/value-own.txt' % DIR, 'machine')], rules
            a.apply()
            wait('A pulled to its own path', lambda: a.read('value-own.txt') == repo.value())
            return 'machine-local > [machine.A] > [os.linux] > shared'
        check('pair-precedence', local_wins)


def cleanup(machine, repository):
    try:
        preview = machine.pb('config', 'unassign', machine.alias, ok=False)
        if isinstance(preview, dict) and preview.get('confirmation_token'):
            machine.pb('config', 'unassign', machine.alias, '--confirm', preview['confirmation_token'], ok=False)
        if repository.get('id'):
            machine.pb('config', 'repository', 'credentials', 'remove', repository['id'], ok=False)
    except Exception as error:  # noqa: BLE001
        print('cleanup %s: %s' % (machine.alias, error), flush=True)
    machine.remove_files()


def main():
    args = [arg for arg in sys.argv[1:] if not arg.startswith('--')]
    sections = '--machine-sections' in sys.argv
    reach = next((arg.split('=', 1)[1] for arg in sys.argv if arg.startswith('--repo-from-controller=')), args[1])
    repo = Repository(args[0])
    a, b = machine(args[2], args[3]), machine(args[4], args[5])
    forwards = contextlib.ExitStack()
    password = base64.b32encode(os.urandom(15)).decode().lower()
    profile = json.dumps({'transport': 'http', 'auth': 'token', 'username': 'cfguser', 'password': password, 'allow_insecure_http': True})
    repository = {}
    try:
        repo.seed({'.paperboat/config-sync.toml': shared_file(a.alias, sections, sorted({a.os, b.os})), 'cfg/value.txt': 'seed\n'})
        port = repo.serve(password)
        url = 'http://%s:%d/pair.git' % (args[1], port)
        for side in (a, b):
            if side.os == 'windows':
                side.own_url = 'http://127.0.0.1:%d/pair.git' % forwards.enter_context(side.device.forward(port, reach))
        run_checks(repo, a, b, url, profile, password, sections, repository)
    except Exception:  # noqa: BLE001
        pass
    finally:
        for side in (a, b):
            cleanup(side, repository)
        if repository.get('id'):
            a.delete_repository(repository['id'])
        forwards.close()
        repo.close()
    failed = [name for name, outcome in results.items() if outcome == 'FAIL']
    status = 'FAIL' if failed or not results else 'PASS'
    print(json.dumps({'check': 'config-sync-pair', 'status': status, 'results': results}), flush=True)
    sys.exit(1 if status == 'FAIL' else 0)


if __name__ == '__main__':
    main()
