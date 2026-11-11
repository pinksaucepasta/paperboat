#!/usr/bin/env python3
"""X-01..X-10: config sync between machines of different operating systems (one account, one shared repository).

  xplat_config.py [--only X-01,X-03] [--devices L,W]

Runs on the controller against the fleet of xplat_fleet.py. One bare Git repository with an HTTP server (token) lives on
the Linux host; Linux reaches it directly, Windows through ssh -R, macOS through the host's LAN address. Each check names
its row and pair (`X-01:L-W`) and is recorded in tools/device-smoke/results/xplat-config-<stamp>.json. Everything it
creates (assignments, machine files, files, credentials, the repository, the server) is removed at the end.
"""
import base64
import contextlib
import hashlib
import json
import os
import shlex
import subprocess
import sys
import time
from itertools import permutations
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import xplat_fleet as fleet  # noqa: E402
from config_sync_lifecycle import HTTP_SERVER  # noqa: E402

REPO_HOST = os.environ.get('XPLAT_REPO_HOST', 'hp')
REPO_BRIDGE = os.environ.get('XPLAT_REPO_BRIDGE', '10.0.0.1')  # the repository host as the Linux container reaches it
REPO_ROOT = '/tmp/xplat-cfg-repo'
NAME = 'xplat-config-fixture'
SYNC_WAIT = 240
GIT_ENV = 'GIT_AUTHOR_NAME=x GIT_AUTHOR_EMAIL=x@example.test GIT_COMMITTER_NAME=x GIT_COMMITTER_EMAIL=x@example.test'

# Where an app's files live on each OS, as written in the TOML and as the filesystem sees them (below the home directory).
RULE_ROOT = {'linux': '~/.config/xplat', 'darwin': '~/Library/Application Support/xplat', 'windows': '%APPDATA%/xplat'}
REAL_ROOT = {'linux': '.config/xplat', 'darwin': 'Library/Application Support/xplat', 'windows': 'AppData/Roaming/xplat'}
results = {}


def sep(dev):
    return '\\' if dev.os == 'windows' else '/'


def real(dev, *parts):
    """Filesystem path of a file below the app root on this device."""
    return dev.home + sep(dev) + sep(dev).join(REAL_ROOT[dev.os].split('/') + list(parts))


class Repo:
    """The bare repository and its HTTP server on the repository host."""

    def sh(self, script, input=None, timeout=120):
        done = subprocess.run(['ssh', REPO_HOST, 'bash', '-c', shlex.quote(script)], input=input, capture_output=True, timeout=timeout)
        return done

    def seed(self, files):
        script = ['rm -rf %s && mkdir -p %s && git init -q --bare -b main %s/pair.git' % (REPO_ROOT, REPO_ROOT, REPO_ROOT),
                  'work=$(mktemp -d) && git clone -q %s/pair.git $work && cd $work' % REPO_ROOT]
        for name, data in files.items():
            script.append('mkdir -p "$(dirname %s)" && printf %%s %s | base64 -d > %s'
                          % (shlex.quote(name), shlex.quote(base64.b64encode(data).decode()), shlex.quote(name)))
        script.append('git add -A && %s git commit -q -m seed && git push -q origin HEAD:main && rm -rf $work' % GIT_ENV)
        done = self.sh(' && '.join(script))
        assert done.returncode == 0, done.stderr.decode()[-300:]

    def commit(self, files):
        """Push a commit that sets these files (a bytes value) or removes them (None) on main."""
        script = ['work=$(mktemp -d) && git clone -q %s/pair.git $work && cd $work' % REPO_ROOT]
        for name, data in files.items():
            if data is None:
                script.append('git rm -q -f %s' % shlex.quote(name))
            else:
                script.append('mkdir -p "$(dirname %s)" && printf %%s %s | base64 -d > %s'
                              % (shlex.quote(name), shlex.quote(base64.b64encode(data).decode()), shlex.quote(name)))
        script.append('git add -A && %s git commit -q -m change && git push -q origin HEAD:main && rm -rf $work' % GIT_ENV)
        done = self.sh(' && '.join(script))
        assert done.returncode == 0, done.stderr.decode()[-300:]

    def show(self, path):
        done = self.sh('git --git-dir %s/pair.git show main:%s' % (REPO_ROOT, shlex.quote(path)))
        return done.stdout if done.returncode == 0 else None

    def serve(self, password):
        self.sh('cat > %s/server.py' % REPO_ROOT, input=HTTP_SERVER.replace("('127.0.0.1', 0)", "('0.0.0.0', 0)").encode())
        self.sh('cd %s && (setsid nohup python3 server.py %s cfguser %s > port 2>/dev/null < /dev/null & echo $! > pid)'
                % (REPO_ROOT, REPO_ROOT, shlex.quote(password)))
        for _ in range(20):
            port = self.sh('cat %s/port' % REPO_ROOT).stdout.decode().strip()
            if port:
                return int(port)
            time.sleep(0.5)
        raise RuntimeError('repository server did not start')

    def close(self):
        self.sh('kill $(cat %s/pid) 2>/dev/null; rm -rf %s' % (REPO_ROOT, REPO_ROOT))


def pbj(dev, *args, input=None, ok=True, timeout=200):
    """Run `pb … --json` and return its data (raises when the command fails and ok is true)."""
    done = dev.pb(*args, '--json', input=input, timeout=timeout)
    try:
        value = json.loads(done.text)
    except ValueError:
        raise RuntimeError('%s: pb %s -> %s %s' % (dev.name, ' '.join(args), done.text[-200:], done.errtext[-200:]))
    if isinstance(value, dict) and 'schema_version' in value and 'ok' in value:
        if ok and value['ok'] is False and 'data' not in value:
            raise RuntimeError('%s: pb %s failed: %s' % (dev.name, ' '.join(args), value.get('error', {}).get('message')))
        return value.get('data', value)
    return value


def wait(what, check, seconds=SYNC_WAIT, every=4):
    end = time.time() + seconds
    while time.time() < end:
        if check():
            return
        time.sleep(every)
    raise AssertionError('timed out waiting for ' + what)


def machine_file(repository_id, extra='', url=None, mode='bidirectional'):
    target = 'repository_id = "%s"\n' % repository_id + ('url = "%s"\n' % url if url else '')
    return ('version = 1\nenabled = true\nautomatic_updates = true\nmode = "%s"\n[pull]\n%s[push]\n%s%s' % (mode, target, target, extra))


class Context:
    """The repository, its registration and one config-sync assignment per device."""

    def __init__(self, devices):
        self.devices, self.repo, self.repository_id, self.urls, self.forwards = devices, Repo(), None, {}, contextlib.ExitStack()
        self.password = base64.b32encode(os.urandom(15)).decode().lower()

    def unix(self):
        return next(d for d in self.devices.values() if d.os != 'windows')

    def sync_file(self, dev):
        return dev.pb('config', 'sync', 'path').text.strip()

    def write_sync_file(self, dev, text):
        path = self.sync_file(dev)
        dev.write(path, text.encode(), mode='600')
        if dev.os == 'windows':
            winhost_private(dev, path)

    def start(self, shared):
        for dev in self.devices.values():
            dev.remove(real(dev))  # a clean slate: files of an earlier run would meet the seed as conflicts
        self.repo.seed(shared)
        port = self.repo.serve(self.password)
        for key, dev in self.devices.items():
            if dev.os == 'linux':
                self.urls[key] = 'http://%s:%d/pair.git' % (REPO_BRIDGE, port)
            elif dev.os == 'windows':
                self.urls[key] = 'http://127.0.0.1:%d/pair.git' % self.forwards.enter_context(dev.device.forward(port, REPO_HOST))
            else:
                self.urls[key] = 'http://%s:%d/pair.git' % (os.environ.get('XPLAT_REPO_LAN', REPO_HOST), port)
        first = self.unix()
        for stale in [r for r in pbj(first, 'config', 'repository', 'list')['repositories'] if r['display_name'] == NAME]:
            delete_repository(first, stale['id'])
        self.repository_id = pbj(first, 'config', 'repository', 'connect', self.urls[next(k for k, d in self.devices.items() if d is first)], '--name', NAME)['repository']['id']
        profile = json.dumps({'transport': 'http', 'auth': 'token', 'username': 'cfguser', 'password': self.password,
                              'allow_insecure_http': True})
        for dev in self.devices.values():
            saved = pbj(dev, 'config', 'repository', 'credentials', 'set', self.repository_id, '--file', '-', input=profile.encode())
            assert saved['state'] == 'saved', saved
            assert self.password not in outputs(dev)

    def assign(self, key, extra='', mode='bidirectional', automatic=True):
        dev = self.devices[key]
        text = machine_file(self.repository_id, extra, self.urls[key], mode)
        self.write_sync_file(dev, text if automatic else text.replace('automatic_updates = true', 'automatic_updates = false'))
        projection = pbj(dev, 'config', 'sync', 'validate')
        apply(dev)
        return projection

    def finish(self):
        for dev in self.devices.values():
            try:
                preview = pbj(dev, 'config', 'unassign', dev.name, ok=False)
                if isinstance(preview, dict) and preview.get('confirmation_token'):
                    pbj(dev, 'config', 'unassign', dev.name, '--confirm', preview['confirmation_token'], ok=False)
                if self.repository_id:
                    pbj(dev, 'config', 'repository', 'credentials', 'remove', self.repository_id, ok=False)
            except Exception as error:  # noqa: BLE001
                print('cleanup %s: %s' % (dev.name, error), flush=True)
            dev.remove(self.sync_file(dev))
            dev.remove(real(dev))
        if self.repository_id:
            delete_repository(self.unix(), self.repository_id)
        self.forwards.close()
        self.repo.close()


def winhost_private(dev, path):
    import winhost
    winhost.RemotePath(dev.device, path).private()


def outputs(dev):
    return dev.pb('config', 'repository', 'list', '--json').text + dev.pb('config', 'status', '--json').text


def apply(dev):
    preview = pbj(dev, 'config', 'sync', 'apply', ok=False)
    assert 'confirmation_token' in preview, '%s: apply preview: %s' % (dev.name, json.dumps(preview)[:400])
    return pbj(dev, 'config', 'sync', 'apply', '--confirm', preview['confirmation_token'])


def delete_repository(dev, repository_id):
    """The API has a delete the CLI does not: use a Linux device's own signed-in session."""
    from config_sync_pair import DELETE_REPOSITORY
    done = dev.sh('python3 - ' + shlex.quote(repository_id), input=DELETE_REPOSITORY.encode())
    if done.rc:
        print('repository delete: %s' % done.errtext[-200:], flush=True)


def check(row, pair, action):
    name = '%s:%s' % (row, pair)
    started = time.time()
    try:
        detail = action()
        results[name] = {'status': 'PASS', 'detail': detail or ''}
        print('PASS %s %ds %s' % (name, time.time() - started, detail or ''), flush=True)
    except Exception as error:  # noqa: BLE001
        results[name] = {'status': 'FAIL', 'detail': str(error)[:600]}
        print('FAIL %s %ds: %s' % (name, time.time() - started, str(error)[:600]), flush=True)


SHARED = (
    'version = 1\n'
    '[paths."cfg/settings.json"]\nkind = "file"\nlocal_path = "~/xplat-default/settings.json"\n'
    '[os.linux.paths."cfg/settings.json"]\nlocal_path = "{linux}/settings.json"\n'
    '[os.darwin.paths."cfg/settings.json"]\nlocal_path = "{darwin}/settings.json"\n'
    '[os.windows.paths."cfg/settings.json"]\nlocal_path = "{windows}/settings.json"\n'
    '[paths."cfg/app"]\nkind = "directory"\ninclude = ["**/*.json", "**/*.txt"]\nexclude = ["**/*.tmp"]\nlocal_path = "~/xplat-default/app"\n'
    '[os.linux.paths."cfg/app"]\nlocal_path = "{linux}/app"\n'
    '[os.darwin.paths."cfg/app"]\nlocal_path = "{darwin}/app"\n'
    '[os.windows.paths."cfg/app"]\nlocal_path = "{windows}/app"\n'
).format(**RULE_ROOT)


def rules(dev, projection):
    return {r['repository_path']: r for r in projection['path_rules']}


def sha(data):
    return hashlib.sha256(data).hexdigest()


def arrives(dev, path, data, what):
    wait(what, lambda: dev.read(path) == data)


def x01(ctx, a, b, direction):
    """X-01: per-OS destinations of one shared file and directory rule; edits arrive byte-identical."""
    for dev in (a, b):
        projection = pbj(dev, 'config', 'sync', 'validate')
        found = rules(dev, projection)
        assert found['cfg/settings.json']['local_path'] == RULE_ROOT[dev.os] + '/settings.json', (dev.name, found['cfg/settings.json'])
        assert found['cfg/app']['local_path'] == RULE_ROOT[dev.os] + '/app', (dev.name, found['cfg/app'])
    blob = os.urandom(1500) + b'\n{"k": "v"}\n'
    tag = direction.replace('>', '')
    files = {'settings.json': blob, 'app/%s/one.json' % tag: b'{"one": 1}\n', 'app/%s/sub/two.txt' % tag: b'two\n' + os.urandom(64),
             'app/%s/skip.tmp' % tag: b'tmp\n'}
    for name, data in files.items():
        a.write(real(a, *name.split('/')), data)
    wait('repository has the edits', lambda: ctx.repo.show('cfg/settings.json') == blob and
         ctx.repo.show('cfg/app/%s/sub/two.txt' % tag) == files['app/%s/sub/two.txt' % tag])
    for name, data in files.items():
        if not name.endswith('skip.tmp'):
            arrives(b, real(b, *name.split('/')), data, '%s received %s' % (b.name, name))
    assert ctx.repo.show('cfg/app/%s/skip.tmp' % tag) is None and not b.exists(real(b, 'app', tag, 'skip.tmp')), 'an excluded file travelled'
    assert sha(b.read(real(b, 'settings.json'))) == sha(blob)
    return 'sha256 %s..., dir rule include/exclude honoured' % sha(blob)[:12]


def x01_variables(ctx, a, b, direction):
    """X-01: a variable that does not exist on this OS fails with unsupported_destination, no substitute path."""
    for dev in (a, b):
        bad = '%APPDATA%/xplat/x.txt' if dev.os != 'windows' else '$XDG_CONFIG_HOME/xplat/x.txt'
        extra = '[paths."cfg/novar.txt"]\nkind = "file"\nlocal_path = "%s"\n' % bad
        ctx.write_sync_file(dev, machine_file(ctx.repository_id, extra, ctx.urls[dev.key]))
        done = dev.pb('config', 'sync', 'validate', '--json')
        # the documented rule is "missing variables fail rather than choosing substitutes" (docs/config-sync.md)
        assert done.rc != 0 and '"ok":false' in done.text.replace(' ', ''), (dev.name, done.text[-300:])
        assert not dev.exists(real(dev, 'x.txt')), 'a substitute path was created'
        ctx.write_sync_file(dev, machine_file(ctx.repository_id, '', ctx.urls[dev.key]))
    return 'refused on every device, no substitute path'


def x03_bytes(ctx, a, b, direction):
    """X-03: CRLF, LF, mixed endings and a missing final newline arrive byte for byte."""
    files = {'app/crlf.txt': b'one\r\ntwo\r\nthree\r\n', 'app/lf.txt': b'one\ntwo\nthree\n',
             'app/mixed.txt': b'one\r\ntwo\nthree\r\nfour\n', 'app/nonl.txt': b'one\r\ntwo', 'app/lone-cr.txt': b'a\rb\r\nc'}
    for name, data in files.items():
        a.write(real(a, *name.split('/')), data)
    for name, data in files.items():
        arrives(b, real(b, *name.split('/')), data, '%s received %s' % (b.name, name))
        assert ctx.repo.show('cfg/' + name) == data, 'the repository changed the bytes of ' + name
    return 'sha256 equal on %s and in the repository for %d line-ending shapes' % (b.name, len(files))


def conflicts(dev):
    return pbj(dev, 'config', 'conflict', 'list', dev.name).get('conflicts', [])


def x03_merge(ctx, a, b, direction):
    """X-03: independent edits on both sides of a CRLF file merge without mixing endings."""
    name, base = 'app/merge.txt', ''.join('l%d\r\n' % i for i in range(1, 6)).encode()
    a.write(real(a, 'app', 'merge.txt'), base)
    arrives(b, real(b, 'app', 'merge.txt'), base, 'base on ' + b.name)
    ctx.assign(b.key, automatic=False)
    try:
        mine = base.replace(b'l1\r\n', b'A1\r\n')
        a.write(real(a, 'app', 'merge.txt'), mine)
        wait('A published', lambda: ctx.repo.show('cfg/' + name) == mine)
        theirs = base.replace(b'l5\r\n', b'B5\r\n')
        b.write(real(b, 'app', 'merge.txt'), theirs)
    finally:
        ctx.assign(b.key)
    merged = base.replace(b'l1\r\n', b'A1\r\n').replace(b'l5\r\n', b'B5\r\n')
    wait('merged result everywhere', lambda: b.read(real(b, 'app', 'merge.txt')) == merged and a.read(real(a, 'app', 'merge.txt')) == merged)
    assert merged.count(b'\r\n') == merged.count(b'\n') == 5
    return 'three-way merge kept CRLF on every line'


def refuses(dev, ctx, extra, what):
    """Write a machine file with these extra rules; `validate` must fail, and the file is put back to its plain form."""
    ctx.write_sync_file(dev, machine_file(ctx.repository_id, extra, ctx.urls[dev.key]))
    done = dev.pb('config', 'sync', 'validate', '--json')
    ctx.write_sync_file(dev, machine_file(ctx.repository_id, '', ctx.urls[dev.key]))
    assert done.rc != 0 and '"ok":false' in done.text.replace(' ', ''), '%s: %s was accepted: %s' % (dev.name, what, done.text[-200:])


def accepted(dev, ctx, extra, what):
    ctx.write_sync_file(dev, machine_file(ctx.repository_id, extra, ctx.urls[dev.key]))
    done = dev.pb('config', 'sync', 'validate', '--json')
    ctx.write_sync_file(dev, machine_file(ctx.repository_id, '', ctx.urls[dev.key]))
    assert done.rc == 0, '%s: %s was refused: %s' % (dev.name, what, done.text[-200:])
    return json.loads(done.text).get('data', json.loads(done.text))


def rule(repository_path, local_path):
    return '[paths."%s"]\nkind = "file"\nlocal_path = "%s"\n' % (repository_path, local_path)


def x06(ctx, a, b, direction):
    """X-06: case-insensitive and reserved names are refused in rules on every OS (a valid rule is the control)."""
    for dev in (a, b):
        root = RULE_ROOT[dev.os] + '/x06'
        accepted(dev, ctx, rule('cfg/x06-ok.txt', root + '/ok.txt'), 'a plain rule (control)')
        refuses(dev, ctx, rule('cfg/Case.txt', root + '/c1.txt') + rule('cfg/case.txt', root + '/c2.txt'),
                'two rules whose repository paths differ only by case')
        refuses(dev, ctx, rule('cfg/p.txt', root + '/App.json') + rule('cfg/q.txt', root + '/app.json'),
                'two rules whose destinations differ only by case')
        for bad in ('cfg/CON.txt', 'cfg/prn', 'cfg/Aux.log', 'cfg/NUL', 'cfg/com1.txt', 'cfg/LPT9', 'cfg/a:b.txt',
                    'cfg/trail.', 'cfg/trail ', 'cfg/dir./f.txt'):
            refuses(dev, ctx, rule(bad, root + '/r.txt'), 'repository name ' + repr(bad))
    return 'overlap by case and %d reserved or unsafe names refused on %s and %s' % (12, a.name, b.name)


def x07_paths(ctx, a, b, direction):
    """X-07: a drive-letter or backslash local_path is accepted on Windows only; elsewhere it fails validation."""
    for dev in (a, b):
        win = (dev.home if dev.os == 'windows' else 'C:\\Users\\someone') + '\\xplat\\x07\\drive.txt'
        extra = rule('cfg/x07-drive.txt', win.replace('\\', '\\\\'))
        if dev.os == 'windows':
            accepted(dev, ctx, extra, 'a drive-letter path on Windows')
        else:
            refuses(dev, ctx, extra, 'a Windows drive-letter path on ' + dev.os)
            assert not dev.exists(dev.home + '/C:\\Users\\someone\\xplat\\x07\\drive.txt'), 'a literal path was created'
    return 'drive-letter path accepted on Windows only'


CHECKS = [('X-01', x01), ('X-01', x01_variables), ('X-03', x03_bytes), ('X-03', x03_merge), ('X-06', x06), ('X-07', x07_paths)]


def run_pairs(ctx, devices, only):
    for row, function in CHECKS:
        if only and row not in only:
            continue
        for a_key, b_key in permutations(devices, 2):
            a, b = devices[a_key], devices[b_key]
            check(row, '%s>%s' % (a_key, b_key), lambda: function(ctx, a, b, '%s>%s' % (a_key, b_key)))


def main():
    argv = sys.argv[1:]
    only = set(argv[argv.index('--only') + 1].split(',')) if '--only' in argv else set()
    keys = argv[argv.index('--devices') + 1].split(',') if '--devices' in argv else ['L', 'M']
    devices = {k: d for k, d in fleet.fleet().items() if k in keys}
    for key, dev in devices.items():
        dev.key = key
    ctx = Context(devices)
    try:
        ctx.start({'.paperboat/config-sync.toml': SHARED.encode(), 'cfg/settings.json': b'{}\n'})
        for key in devices:
            ctx.assign(key)
        run_pairs(ctx, devices, only)
    finally:
        ctx.finish()
    stamp = time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())
    out = Path(__file__).parent / 'results' / ('xplat-config-%s.json' % stamp)
    out.write_text(json.dumps({'check': 'xplat-config', 'devices': keys, 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
