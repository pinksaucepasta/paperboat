#!/usr/bin/env python3
"""Replace the managed API only after readiness; retain live old connections."""
import argparse
import fcntl
import json
import os
from pathlib import Path
import re
import subprocess
import time

# Logs of retired server containers are kept this long under rollout-logs/.
LOG_RETENTION_SECONDS = 7 * 24 * 3600
# Server images kept besides the deployed one (rollback targets); older ones and all build cache are
# removed after each rollout. Unbounded, they filled the disk once (96 images and 50 GB of build
# cache, 99% full, 2026-10-23). An age filter (24 h) still let one evening's builds grow 22 GB, and
# on the containerd image store (Docker 29) size bounds (--keep-storage, --max-used-space) free
# nothing, so the whole cache goes: the next build re-downloads Go modules (about a minute).
PREVIOUS_IMAGES_KEPT = 2


class RolloutError(Exception):
    pass


def connection_count(contents):
    """Count accepted live TCP sockets on this process namespace's listen ports."""
    rows = [line.split() for content in contents for line in content.splitlines()[1:]]
    ports = {row[1].rsplit(':', 1)[1] for row in rows if len(row) > 3 and row[3] == '0A'}
    return sum(len(row) > 3 and row[1].rsplit(':', 1)[1] in ports
               and row[3] not in ('0A', '06', '07') for row in rows)


class Rollout:
    def __init__(self, directory):
        self.root = Path(directory)
        self.state_path = self.root / 'server-rollout.json'
        self.journal = self.root / 'server-rollout-pending.json'
        self.log = (self.root / 'server-rollout.log').open('ab')

    def run(self, args, image=None, timeout=120):
        env = dict(os.environ)
        if image:
            env['PAPERBOAT_SERVER_IMAGE'] = image
        result = subprocess.run(args, cwd=self.root, env=env, stdout=subprocess.PIPE,
                                stderr=self.log, timeout=timeout)
        if result.returncode:
            self.log.write(result.stdout)
            self.log.flush()
            raise RolloutError('Deployment step failed: ' + ' '.join(args[:3])
                               + '. Inspect the protected server-rollout.log.')
        return result.stdout

    def save(self, path, value):
        temporary = path.with_suffix('.next')
        temporary.write_text(json.dumps(value) + '\n')
        temporary.replace(path)

    def current(self):
        if self.state_path.exists():
            return json.loads(self.state_path.read_text())
        container = self.run(['docker', 'compose', 'ps', '-q', 'server']).decode().strip()
        if not container:
            raise RolloutError('No running initial server. Bootstrap Compose first.')
        info = json.loads(self.run(['docker', 'inspect', container]))[0]
        return {'active': info['Name'].lstrip('/'), 'retired': []}

    def ready(self, name):
        # The container needs a database and provider configuration, not just a
        # bound socket. Do not log response bodies or resolved environment.
        try:
            self.run(['docker', 'exec', name, 'curl', '-fsS', '--max-time', '3',
                      'http://127.0.0.1:8080/readyz'], timeout=5)
            return True
        except (RolloutError, subprocess.TimeoutExpired):
            return False

    def reload(self):
        self.run(['docker', 'compose', 'exec', '-T', 'proxy', 'caddy', 'reload',
                  '--config', '/etc/caddy/Caddyfile'], timeout=30)

    def reap(self, state):
        retained = []
        for name in state['retired']:
            # Read-only kernel state includes HTTP/2, long responses, and control
            # streams. A retirement deadline must never kill an admitted stream.
            info = json.loads(self.run(['docker', 'inspect', name]))[0]
            if info['State']['Running']:
                sockets = [self.run(['docker', 'exec', name, 'cat', path]).decode()
                           for path in ('/proc/net/tcp', '/proc/net/tcp6')]
                if connection_count(sockets):
                    retained.append(name)
                    continue
                self.run(['docker', 'stop', '--time', '30', name], timeout=40)
            self.keep_logs(name)
            self.run(['docker', 'rm', name])
        state['retired'] = retained
        self.save(self.state_path, state)
        return len(retained)

    def keep_logs(self, name):
        # A request answered by a draining server must stay traceable after the
        # container is removed. Best effort: a failed copy never blocks retirement.
        logs = self.root / 'rollout-logs'
        try:
            logs.mkdir(mode=0o700, exist_ok=True)
            cutoff = time.time() - LOG_RETENTION_SECONDS
            for old in logs.glob('*.log'):
                if old.stat().st_mtime < cutoff:
                    old.unlink()
            with open(logs / (name + '.log'), 'wb', opener=lambda p, f: os.open(p, f, 0o600)) as out:
                subprocess.run(['docker', 'logs', name], cwd=self.root, stdout=out,
                               stderr=subprocess.STDOUT, timeout=120)
        except (OSError, subprocess.SubprocessError) as error:
            self.log.write(('Could not keep logs of %s: %s\n' % (name, error)).encode())
            self.log.flush()

    def prune_images(self, deployed):
        # Best effort: a failed prune never fails a rollout that is already serving.
        try:
            used = set(self.run(['docker', 'ps', '-a', '--format', '{{.Image}}']).decode().split())
            rows = self.run(['docker', 'image', 'ls', 'paperboat-server', '--format',
                             '{{.Repository}}:{{.Tag}} {{.CreatedAt}}']).decode().splitlines()
            images = [row.split(' ', 1) for row in rows if ' ' in row]
            images = [row for row in images if not row[0].endswith(':<none>')]
            images.sort(key=lambda row: row[1], reverse=True)
            kept = 0
            for name, _ in images:
                if name == deployed or name in used:
                    continue
                if kept < PREVIOUS_IMAGES_KEPT:
                    kept += 1
                    continue
                self.run(['docker', 'rmi', name])
            self.run(['docker', 'image', 'prune', '-f'])
            self.run(['docker', 'builder', 'prune', '-af'], timeout=600)
        except (RolloutError, subprocess.SubprocessError) as error:
            self.log.write(('Could not prune images: %s\n' % error).encode())
            self.log.flush()

    def recover(self):
        if not self.journal.exists():
            return
        record = json.loads(self.journal.read_text())
        old = record['before']
        if not self.ready(old['active']):
            raise RolloutError('Interrupted rollout: previous server is not ready;'
                               ' retain both containers and inspect the protected journal.')
        # On interruption restore routing before touching either process. A new
        # server may already hold live streams, so retain it until they finish.
        (self.root / 'Caddyfile').write_text(record['caddy'])
        self.reload()
        (self.root / '.env').write_text(record['env'])
        old['retired'].append(record['candidate'])
        self.save(self.state_path, old)
        self.journal.unlink()

    def deploy(self, image):
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._/@:-]{0,511}", image):
            raise RolloutError("Use one valid Docker image reference.")
        self.recover()
        before = self.current()
        if self.reap(before) >= 2:
            raise RolloutError('Two previous servers still hold live connections. '
                               'Run --reap after those sessions finish; none were stopped.')
        info = json.loads(self.run(['docker', 'image', 'inspect', image]))[0]
        active = json.loads(self.run(['docker', 'inspect', before['active']]))[0]
        if active['Image'] == info['Id']:
            return 'Current server already runs this image.'
        candidate = 'paperboat-server-' + info['Id'].split(':')[-1][:16]
        # Additive migrations run while the current version is still serving.
        self.run(['docker', 'compose', 'run', '--rm', '--no-deps', 'migrate'],
                 image=image, timeout=300)
        existing = self.run(['docker', 'container', 'ls', '-a', '--filter',
                             'name=^/' + candidate + '$', '--format', '{{.Names}}'])
        if existing.strip():
            raise RolloutError('Replacement container name already exists; nothing was removed.')
        try:
            self.run(['docker', 'compose', 'run', '-d', '--no-deps', '--name', candidate,
                      'server'], image=image)
            for _ in range(60):
                if self.ready(candidate):
                    break
                time.sleep(1)
            else:
                raise RolloutError('Replacement server did not become ready; previous server retained.')
            self.run(['docker', 'update', '--restart', 'unless-stopped', candidate])
            caddy = (self.root / 'Caddyfile').read_text()
            env = (self.root / '.env').read_text()
            updated, count = re.subn(r'reverse_proxy (?:server|'
                                    + re.escape(before['active']) + r'):8080\b',
                                    'reverse_proxy ' + candidate + ':8080', caddy)
            if count != 2:
                raise RolloutError('Expected API and installer upstreams; routing was not changed.')
            next_env, count = re.subn(r'^PAPERBOAT_SERVER_IMAGE=.*$',
                                     'PAPERBOAT_SERVER_IMAGE=' + image, env, flags=re.M)
            if count != 1:
                raise RolloutError('Expected one managed server image setting; routing was not changed.')
            self.save(self.journal, {'before': before, 'candidate': candidate,
                                     'caddy': caddy, 'env': env})
            # Preserve the bind-mounted inode. Replacing the host file would leave
            # the proxy reading its previous inode during reload.
            (self.root / 'Caddyfile').write_text(updated)
            self.reload()
            (self.root / '.env').write_text(next_env)
            state = {'active': candidate, 'retired': before['retired'] + [before['active']]}
            self.save(self.state_path, state)
            self.journal.unlink()
        except BaseException:
            if self.journal.exists():
                self.recover()
            else:
                # A pre-routing candidate cannot hold admitted user traffic.
                self.run(['docker', 'rm', '-f', candidate])
            raise
        # Traffic has moved: the retired server stops its background workers and hands worker
        # leadership to the replacement; it keeps serving the connections it holds (F21-04).
        # Outside the rollback path on purpose: a failed signal must never remove the new server.
        try:
            self.run(['docker', 'kill', '--signal=SIGUSR1', before['active']])
        except RolloutError as error:
            print('Warning: retired server was not told to drain its workers:', error)
        retained = self.reap(state)
        self.prune_images(image)
        return 'Ready replacement is serving; retained draining servers: ' + str(retained)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--directory', default='/opt/paperboat')
    parser.add_argument('--image')
    parser.add_argument('--reap', action='store_true')
    args = parser.parse_args()
    if bool(args.image) == args.reap:
        parser.error('choose --image or --reap')
    os.umask(0o077)
    root = Path(args.directory)
    with (root / 'server-rollout.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        rollout = Rollout(root)
        try:
            if args.reap:
                rollout.recover()
                print('Draining servers retained:', rollout.reap(rollout.current()))
            else:
                print(rollout.deploy(args.image))
        finally:
            rollout.log.close()


if __name__ == '__main__':
    try:
        main()
    except (RolloutError, subprocess.TimeoutExpired, BlockingIOError) as error:
        print(str(error))
        raise SystemExit(1)
