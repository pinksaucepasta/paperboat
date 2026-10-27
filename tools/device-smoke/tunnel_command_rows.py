#!/usr/bin/env python3
"""Verify tunnel command main/error paths on one disposable enrolled Linux machine."""
import argparse
import functools
import http.server
import json
import os
from pathlib import Path
import tempfile
import threading

from install_lifecycle import registration
import pairing_lifecycle
from pairing_lifecycle import command_result, confirmed_command
from preview_lifecycle import QuietHandler
from tunnel_domain import FixtureDns, BODY, ephemeral, fetch, wait


class CheckFailure(RuntimeError):
    pass


def require(value, message):
    if not value:
        raise CheckFailure(message)


def redacted(value):
    if isinstance(value, dict):
        return {k: ('<redacted>' if any(s in k.lower() for s in
                    ('token', 'password', 'secret', 'private_key', 'authorization', 'cookie', 'credential', 'enrollment'))
                    else redacted(v)) for k, v in value.items()}
    if isinstance(value, list):
        return [redacted(v) for v in value]
    return value


class Suite:
    def __init__(self, args):
        self.args = args
        self.pb = Path.home() / '.local/bin/pb'
        self.before = registration()
        self.rows = {f'C{i:03d}': {'main': False, 'error': False, 'failures': []}
                     for i in range(98, 124)}
        self.rows['C242'] = {'main': False, 'error': False, 'failures': []}
        self.last = None
        self.tunnel = self.extra = self.policy = self.capture = None
        self.dns = []
        self.zone_dns = None
        self.name = args.run
        self.missing = 'missing-' + args.run
        self.hostname = args.run + '.' + args.zone
        pairing_lifecycle.command_result = self.observe

    def observe(self, binary, *argv, **kwargs):
        safe_argv = list(argv)
        if '--confirm' in safe_argv:
            safe_argv[safe_argv.index('--confirm') + 1] = '<redacted>'
        self.last = {'argv': [str(binary), *safe_argv]}
        result = command_result(binary, *argv, **kwargs)
        require(len(result.stdout) + len(result.stderr) <= 2 << 20, 'output exceeds fixture bound')
        try:
            stdout = json.dumps(redacted(json.loads(result.stdout)))
        except ValueError:
            stdout = result.stdout
        self.last.update(exit=result.returncode, stdout=stdout, stderr=result.stderr)
        return result

    def command(self, *words, expect_error=False, json_output=True, timeout=120):
        argv = ['tunnel', *map(str, words)] + (['--json'] if json_output else [])
        result = self.observe(self.pb, *argv, timeout=timeout)
        try:
            value = json.loads(result.stdout)
        except ValueError:
            value = None
        text = result.stdout + result.stderr
        require(not any(x in text for x in ('panic:', 'goroutine ', 'Traceback', 'nil pointer')),
                'command printed a stack trace')
        if expect_error:
            require(result.returncode != 0 and text.strip(), 'error path was not refused clearly')
            return value
        require(result.returncode == 0, 'main path command failed')
        if not json_output:
            require('Usage:' in text, 'group did not show usage')
            return text
        require(isinstance(value, dict) and value.get('ok') is not False, 'invalid success receipt')
        return value.get('data', value)

    def check(self, row, mode, action):
        if self.args.rows and row not in self.args.rows:
            return
        self.last = None
        try:
            action()
            self.rows[row][mode] = True
        except Exception as error:
            self.rows[row]['failures'].append({'mode': mode, 'reason': str(error),
                                                'command': self.last})
        self.save()

    def save(self):
        value = {'check': 'tunnel-command-rows', 'build': self.args.build,
                 'version': self.args.version, 'server': self.args.server,
                 'run': self.args.run, 'rows': self.rows}
        path = Path(self.args.output)
        with os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600), 'w') as out:
            json.dump(value, out)

    def errors(self):
        cases = {
            'C098': ['delete', self.missing], 'C099': ['doctor', self.missing],
            'C100': ['domain', '--unknown-fixture-flag'],
            'C101': ['domain', 'add', self.missing, self.hostname],
            'C102': ['domain', 'instructions', self.missing, self.hostname],
            'C103': ['domain', 'list', self.missing],
            'C104': ['domain', 'remove', self.missing, self.hostname],
            'C105': ['domain', 'verify', self.missing, self.hostname],
            'C106': ['inspect', self.missing], 'C107': ['list', '--limit', '0'],
            'C108': ['logs', self.missing], 'C109': ['pause', self.missing],
            'C110': ['policy', '--unknown-fixture-flag'],
            'C111': ['policy', 'allow', self.before['machine_id'], '0'],
            'C112': ['policy', 'get', self.missing], 'C113': ['policy', 'revoke', self.missing],
            'C114': ['replay', self.missing, 'absent-capture'],
            'C115': ['resume', self.missing], 'C116': ['route', '--unknown-fixture-flag'],
            'C117': ['route', 'add', self.missing, '--name', 'bad', '--to', 'bad-origin'],
            'C118': ['route', 'list', self.missing],
            'C119': ['route', 'remove', self.missing, 'absent-route'],
            'C120': ['route', 'update', self.missing, 'absent-route', '--path', '/x'],
            'C121': ['show', self.missing], 'C122': ['status', self.missing],
            'C123': ['stop', 'preview_' + self.args.run],
            'C242': ['rename', self.missing, self.name + '-renamed'],
        }
        for row, words in cases.items():
            self.check(row, 'error', lambda words=words: self.command(*words, expect_error=True))
        for row, group in [('C100', 'domain'), ('C110', 'policy'), ('C116', 'route')]:
            self.check(row, 'main', lambda group=group: self.command(group, json_output=False))

    def resource(self):
        require(self.tunnel is not None, 'durable tunnel prerequisite failed')
        return self.tunnel

    def routes(self):
        return self.command('route', 'list', self.resource())['items']

    def lifecycle(self):
        tid = self.resource()
        self.check('C121', 'main', lambda: require(self.command('show', tid)['id'] == tid, 'wrong tunnel'))
        self.check('C107', 'main', lambda: require(any(x['id'] == tid for x in
                   self.command('list', '--q', self.name)['items']), 'tunnel missing from list'))
        self.check('C122', 'main', lambda: require(self.command('status', tid)['overall_code'] == 'ready', 'not ready'))
        self.check('C099', 'main', lambda: require(self.command('doctor', tid)['overall_code'] == 'ready', 'not ready'))
        self.check('C108', 'main', lambda: require(self.command('logs', tid)['items'], 'no lifecycle logs'))
        self.check('C106', 'main', self.inspect)
        self.check('C114', 'main', self.replay)
        self.check('C109', 'main', lambda: self.state('pause', 'paused', 'tunnel_paused'))
        self.check('C115', 'main', lambda: self.state('resume', 'active', 'ready'))
        self.check('C111', 'main', self.allow)
        self.check('C112', 'main', lambda: require(self.command('policy', 'get', self.policy['id'])['id'] == self.policy['id'], 'wrong policy'))
        self.check('C113', 'main', self.revoke)
        self.check('C117', 'main', self.add_route)
        self.check('C120', 'main', self.update_route)
        self.check('C118', 'main', lambda: require(any(x['id'] == self.extra for x in self.routes()), 'added route absent'))
        self.check('C119', 'main', self.remove_route)
        self.check('C242', 'main', self.rename)

    def inspect(self):
        tid = self.resource()
        require(self.command('inspect', tid, '--enable', '--raw')['capture'] == 'enabled', 'capture not enabled')
        wait(lambda: fetch(self.endpoint)[0] == 200, 'public tunnel response', 90)
        records = wait(lambda: self.command('inspect', tid).get('records'), 'HTTP capture', 60)
        self.capture = records[0]['id']
        require(records[0]['method'] == 'GET', 'wrong capture method')
        require(self.command('inspect', tid, '--id', self.capture), 'capture detail absent')

    def replay(self):
        require(self.capture, 'capture prerequisite failed')
        require(self.command('replay', self.resource(), self.capture), 'no replay receipt')
        require(self.command('inspect', self.resource(), '--purge'), 'purge did not return receipt')
        require(self.command('inspect', self.resource(), '--disable')['capture'] == 'disabled', 'capture not disabled')

    def state(self, action, desired, code):
        tid = self.resource()
        changed = self.command(action, tid, '--wait', '--timeout', '90s')
        require(changed['tunnel']['id'] == tid and changed['tunnel']['desired_state'] == desired, 'identity/state changed incorrectly')
        require(self.command('status', tid)['overall_code'] == code, 'health does not reflect state')

    def allow(self):
        self.policy = self.command('policy', 'allow', self.before['machine_id'], self.port, '--access', 'private', '--expires', '30m')
        require(self.policy['machine_id'] == self.before['machine_id'] and self.policy['access_mode'] == 'private', 'wrong policy authority')

    def revoke(self):
        policy = self.command('policy', 'get', self.policy['id'])
        require(self.command('policy', 'revoke', policy['id'], '--generation', policy['generation'])['revoked'], 'policy not revoked')
        self.policy = None

    def add_route(self):
        self.extra = self.command('route', 'add', self.resource(), '--name', 'extra', '--path', '/extra', '--priority', '10', '--to', 'http://127.0.0.1:' + self.port)['route']['id']
        require(any(x['id'] == self.extra for x in self.routes()), 'added route missing')

    def update_route(self):
        require(self.extra, 'route prerequisite failed')
        out = self.command('route', 'update', self.resource(), self.extra, '--name', 'edited', '--path', '/edited')['route']
        require(out['id'] == self.extra and out['name'] == 'edited' and out['path_prefix'] == '/edited', 'route update not applied')

    def remove_route(self):
        require(self.extra, 'route prerequisite failed')
        confirmed_command(self.pb, 'tunnel', 'route', 'remove', self.resource(), self.extra)
        require(not any(x['id'] == self.extra for x in self.routes()), 'removed route still present')
        self.extra = None

    def rename(self):
        tid = self.resource()
        out = self.command('rename', tid, self.name + '-renamed')['tunnel']
        require(out['id'] == tid and out['name'] == self.name + '-renamed', 'rename not applied')
        require(self.command('show', self.name + '-renamed')['id'] == tid, 'renamed selector not resolved')

    def domains(self):
        self.check('C101', 'main', self.add_domain)
        self.check('C103', 'main', lambda: require(any(x['hostname'] == self.hostname for x in self.command('domain', 'list', self.resource())['items']), 'domain not listed'))
        self.check('C102', 'main', self.instructions)
        self.check('C105', 'main', self.verify_domain)
        self.check('C104', 'main', self.remove_domain)

    def add_domain(self):
        route = self.routes()[0]['id']
        out = self.command('domain', 'add', self.resource(), self.hostname, '--route', route, '--provider', 'cloudflare')['domain']
        require(out['hostname'] == self.hostname, 'wrong hostname')

    def instructions(self):
        self.guide = self.command('domain', 'instructions', self.resource(), self.hostname)
        require(self.guide['hostname'] == self.hostname and self.guide['records'], 'DNS instructions missing')

    def verify_domain(self):
        require(getattr(self, 'guide', None), 'DNS instructions prerequisite failed')
        self.zone_dns = FixtureDns(self.args.fixtures_token_file, self.args.fixtures_url, self.args.zone)
        for record in self.guide['records']:
            require(record['type'] == 'CNAME', 'unexpected DNS record type')
            self.dns.append(self.zone_dns.add_cname(record['name'], record['value']))
            self.save_dns()
        self.command('domain', 'verify', self.resource(), self.hostname, '--wait', '--timeout', '3m', timeout=240)
        def ready():
            items = self.command('domain', 'list', self.resource())['items']
            return next((x for x in items if x['hostname'] == self.hostname and x['state'] == 'ready' and x['certificate']['state'] == 'ready'), None)
        require(wait(ready, 'verified domain and TLS', 480), 'domain not ready')
        require(wait(lambda: fetch('https://' + self.hostname)[0] == 200, 'custom-domain HTTPS', 180), 'custom domain did not serve')

    def remove_domain(self):
        confirmed_command(self.pb, 'tunnel', 'domain', 'remove', self.resource(), self.hostname, timeout=90)
        require(not any(x['hostname'] == self.hostname for x in self.command('domain', 'list', self.resource())['items']), 'removed domain remains')

    def save_dns(self):
        Path(self.args.output + '.dns').write_text(json.dumps(self.dns))
        os.chmod(self.args.output + '.dns', 0o600)

    def delete(self):
        tid = self.resource()
        confirmed_command(self.pb, 'tunnel', 'delete', tid)
        require(not any(x['id'] == tid and x['desired_state'] != 'deleted' for x in self.command('list', '--q', self.name)['items']), 'deleted tunnel remains live')
        for record in self.dns:
            require(self.zone_dns.exists(record), 'delete changed user DNS')
        self.tunnel = None

    def main(self):
        self.errors()
        with tempfile.TemporaryDirectory(prefix='paperboat-tunnel-command-') as directory:
            (Path(directory) / 'index.html').write_text(BODY)
            server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), functools.partial(QuietHandler, directory=directory))
            worker = threading.Thread(target=server.serve_forever, daemon=True)
            worker.start(); self.port = str(server.server_port)
            try:
                try:
                    created = self.command('create', self.name, '--port', self.port, '--duration', '30m', '--wait', '--timeout', '90s', timeout=150)
                    self.tunnel = created['tunnel']['id']; self.endpoint = created['tunnel']['stable_endpoint']
                    self.lifecycle(); self.domains(); self.check('C098', 'main', self.delete)
                except Exception as error:
                    self.rows['C098']['failures'].append({'mode': 'prerequisite', 'reason': str(error), 'command': self.last})
                self.check('C123', 'main', lambda: ephemeral(self.pb, int(self.port)))
            finally:
                if self.policy:
                    try: self.revoke()
                    except Exception as error: self.rows['C113']['failures'].append({'mode': 'cleanup', 'reason': str(error)})
                # Recover exact-name creates even if the response was lost.
                try:
                    for item in self.command('list', '--q', self.name)['items']:
                        if item['name'] in (self.name, self.name + '-renamed') and item['account_id'] == self.before['account_id'] and item['desired_state'] != 'deleted':
                            confirmed_command(self.pb, 'tunnel', 'delete', item['id'])
                except Exception as error:
                    self.rows['C098']['failures'].append({'mode': 'cleanup', 'reason': str(error)})
                for record in list(self.dns):
                    try:
                        self.zone_dns.remove(record)
                        self.dns.remove(record)
                        self.save_dns()
                    except Exception as error: self.rows['C105']['failures'].append({'mode': 'cleanup', 'reason': str(error)})
                server.shutdown(); server.server_close(); worker.join(timeout=5)
                if not self.dns:
                    Path(self.args.fixtures_token_file).unlink(missing_ok=True)
                self.save()
        passed = [r for r, x in self.rows.items() if x['main'] and x['error'] and not x['failures']]
        selected = self.args.rows or list(self.rows)
        clean = not any(f['mode'] == 'cleanup' for x in self.rows.values() for f in x['failures'])
        success = clean and all(row in passed for row in selected)
        print(json.dumps({'check': 'tunnel-command-rows', 'status': 'PASS' if success else 'FAIL',
                          'passed': passed}))
        return success


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ['run', 'build', 'version', 'server', 'zone', 'fixtures-url', 'fixtures-token-file', 'output']:
        parser.add_argument('--' + name, required=True)
    parser.add_argument('--rows', nargs='+', default=[], help='verify only these command rows')
    args = parser.parse_args()
    allowed = {f'C{i:03d}' for i in range(98, 124)} | {'C242'}
    if not set(args.rows) <= allowed:
        parser.error('unknown command row')
    raise SystemExit(0 if Suite(args).main() else 1)


if __name__ == '__main__':
    main()
