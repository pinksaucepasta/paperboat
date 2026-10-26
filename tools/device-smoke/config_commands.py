#!/usr/bin/env python3
"""Main/error paths for C127–C138, C169/C172/C177/C179–C181 on an enrolled Linux fixture.

Requires a signed-in pb account (also used by config_sync_lifecycle.py to delete the exact test repository),
an unassigned machine without config-sync.toml, Git, and sudo access (password on stdin if needed).
Reuses the existing local-access settings lifecycle and config-sync Git/file helpers. Only synthetic data is
synced. Local access is restored by its owner; assignment, credentials, repository and files are cleaned.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import tempfile
import uuid

import config_sync_lifecycle as sync
from install_lifecycle import registration
from pairing_lifecycle import command_result, confirmed_command, runtime_root
from tunnel_command_rows import redacted

ROWS = [f'C{i:03d}' for i in range(127, 139)] + ['C169', 'C172', 'C177', 'C179', 'C180', 'C181']
GROUPS = {'C127': [], 'C128': ['alias'], 'C132': ['domain'], 'C135': ['proxy']}
SETTINGS = {'C129', 'C130', 'C131', 'C133', 'C134', 'C136', 'C137', 'C138'}
REPOSITORIES = {'C169', 'C172', 'C177', 'C179', 'C180', 'C181'}


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


class Suite:
    def __init__(self, args):
        self.args = args
        self.pb = Path.home() / '.local/bin/pb'
        self.before = registration()
        self.machine = self.before['machine_id']
        self.rows = {row: {'main': False, 'error': False, 'failures': []} for row in ROWS}
        self.last = None
        self.cleanup_errors = []

    def check(self, row, phase, action):
        try:
            action()
            self.rows[row][phase] = True
        except Exception as error:
            self.rows[row]['failures'].append({'phase': phase, 'reason': str(error), 'command': self.last})

    def call(self, *words, stdin='', error=False):
        argv = ['config', *words, '--json']
        self.last = {'argv': [str(self.pb), *argv]}
        result = command_result(self.pb, *argv, input=stdin, timeout=180)
        require(len(result.stdout) + len(result.stderr) <= 2 << 20, 'command output exceeded bound')
        try:
            value = json.loads(result.stdout)
        except ValueError:
            value = None
        self.last.update(exit=result.returncode, receipt=redacted(value))
        require(value is not None, 'command did not return JSON')
        if error:
            require(result.returncode != 0 and value.get('ok') is False, 'invalid input was accepted')
            require(value['error']['code'] == 'invalid_invocation', 'invalid input failed outside argument validation')
            return value
        require(result.returncode == 0 and value.get('ok') is not False, 'main command failed')
        return value.get('data', value)

    def error_paths(self):
        commands = {
            'C127': ['local-access'], 'C128': ['local-access', 'alias'],
            'C129': ['local-access', 'alias', 'set'], 'C130': ['local-access', 'alias', 'unset'],
            'C131': ['local-access', 'apply'], 'C132': ['local-access', 'domain'],
            'C133': ['local-access', 'domain', 'reset'], 'C134': ['local-access', 'domain', 'set'],
            'C135': ['local-access', 'proxy'], 'C136': ['local-access', 'proxy', 'set'],
            'C137': ['local-access', 'proxy', 'unset'], 'C138': ['local-access', 'show'],
            'C169': ['approve'], 'C172': ['conflict', 'list'],
            'C177': ['repository', 'connect'], 'C179': ['repository', 'credentials', 'remove'],
            'C180': ['repository', 'credentials', 'set'], 'C181': ['repository', 'list'],
        }
        for row, words in commands.items():
            # A specific unknown flag cannot accidentally pass because auth, network or sudo is unavailable.
            self.check(row, 'error', lambda words=words: self.call(*words, '--unknown-config-fixture-flag', error=True))

    def groups(self):
        for row, suffix in GROUPS.items():
            def help_path(suffix=suffix):
                argv = ['config', 'local-access', *suffix]
                self.last = {'argv': [str(self.pb), *argv]}
                result = command_result(self.pb, *argv, input='', timeout=30)
                require(result.returncode == 0 and 'Usage:' in result.stdout and 'Available Commands:' in result.stdout,
                        'bare group did not describe its subcommands')
            self.check(row, 'main', help_path)

    def settings(self, password):
        self.last = {'argv': ['python3', 'local_access_settings.py']}
        result = subprocess.run([sys.executable, str(Path(__file__).with_name('local_access_settings.py'))],
                                input=password + '\n', capture_output=True, text=True, timeout=1200)
        require(len(result.stdout) + len(result.stderr) <= 2 << 20, 'settings check output exceeded bound')
        stdout, stderr = result.stdout, result.stderr
        if password:
            stdout, stderr = stdout.replace(password, '<redacted>'), stderr.replace(password, '<redacted>')
        self.last.update(exit=result.returncode, stdout=stdout, stderr=stderr)
        lines = [line for line in result.stdout.splitlines() if line.startswith('{')]
        receipt = json.loads(lines[-1]) if lines else {}
        require(result.returncode == 0 and receipt.get('status') == 'PASS'
                and receipt.get('alias_proxy_apply_reset') and receipt.get('enrollment_preserved'),
                'local-access lifecycle failed; inspect its private command diagnostic')
        # The existing lifecycle asserts each set/apply/reset receipt against effective settings and, after
        # alias/proxy unset, asserts both lists and the domain match the originals. Do not duplicate that owner.
        for row in SETTINGS:
            self.rows[row]['main'] = True

    def state(self):
        states = self.call('status')['environments']
        return next(item for item in states if item['environment_id'] == self.machine or item['machine_id'] == self.machine)

    def conflicts(self):
        return [item for item in self.call('conflict', 'list', self.machine)['conflicts']
                if item['environment_id'] == self.machine]

    def credentials(self, repository, path):
        profile = json.dumps({'transport': 'local', 'auth': 'anonymous'})
        source = path.parent.parent.parent / ('config-commands-' + uuid.uuid4().hex + '.json')
        with os.fdopen(os.open(source, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), 'w') as output:
            output.write(profile)
        try:
            for filename, stdin in [(str(source), ''), ('-', profile)]:
                saved = self.call('repository', 'credentials', 'set', repository, '--file', filename, stdin=stdin)
                require(saved['repository_id'] == repository and saved['state'] == 'saved', 'wrong saved credential receipt')
                require(path.is_file() and stat.S_IMODE(path.stat().st_mode) == 0o600, 'credential profile is not owner-only')
                require(json.loads(path.read_text()) == json.loads(profile), 'saved profile differs from input')
        finally:
            source.unlink()
        self.rows['C180']['main'] = True
        removed = self.call('repository', 'credentials', 'remove', repository)
        require(removed['repository_id'] == repository and removed['state'] == 'removed' and not path.exists(),
                'credential removal did not remove the selected profile')
        self.rows['C179']['main'] = True

    def approval(self, repository, bare, managed):
        sync.write_machine_file(repository, managed, automatic='false')
        confirmed_command(self.pb, 'config', 'sync', 'apply')
        def reviewed():
            state = self.state()
            return state if state.get('error_code') == 'review_required' else None
        pending = sync.wait('review required', reviewed)
        require(not managed.exists(), 'unapproved repository content was applied')
        approved = self.call('approve', self.machine)
        require(approved['environment']['id'] == self.machine and approved['revision'] == pending['remote_revision'],
                'approval did not name the reviewed revision')
        sync.wait('approved file applied', lambda: sync.read(managed) == sync.repository_value(bare))
        sync.wait('approved status healthy', lambda: self.state()['state'] == 'healthy')
        self.rows['C169']['main'] = True

    def populated_conflicts(self, repository, bare, managed):
        sync.write_machine_file(repository, managed)
        confirmed_command(self.pb, 'config', 'sync', 'apply')
        require(sync.systemctl('stop', sync.UNIT).returncode == 0, 'could not stop fixture config worker')
        try:
            managed.write_text('synthetic machine edit\n')
            sync.edit_repository(bare, 'synthetic repository edit\n')
        finally:
            require(sync.systemctl('start', sync.UNIT).returncode == 0, 'could not restart fixture config worker')
        listed = sync.wait('populated conflict list', lambda: self.conflicts())
        require(len(listed) == 1 and listed[0]['conflict']['path'] == 'cfg/value.txt'
                and bool(listed[0]['conflict']['revision']), 'conflict list did not identify the changed path once')
        self.rows['C172']['main'] = True

    def repositories(self, root):
        repository = None
        applied = False
        require(not sync.MACHINE_FILE.exists(), 'fixture already has config-sync.toml; refusing to replace it')
        require(self.state()['state'] == 'disabled', 'fixture already has an active config assignment')
        # Validate cleanup authority before creating a server-owned repository.
        try:
            require(sync.api_token_request('/v1/config-repositories') == 200, 'repository cleanup authority unavailable')
        except Exception:
            raise RuntimeError('requires a signed-in pb account with repository cleanup permission') from None
        old_root = sync.ROOT
        sync.ROOT = root
        bare = name = None
        try:
            bare = sync.new_bare('local', 'synthetic seed\n')
            name = 'config-commands-' + uuid.uuid4().hex[:12]
            connected = self.call('repository', 'connect', str(bare), '--name', name)
            repository = connected['repository']['id']
            require(connected['repository']['external_ref'] == str(bare)
                    and connected['repository']['provider'] == 'git', 'connected repository differs from fixture')
            self.rows['C177']['main'] = True
            listed = self.call('repository', 'list', '--q', name)['repositories']
            require([item['id'] for item in listed] == [repository], 'filtered list lost or changed repository identity')
            self.rows['C181']['main'] = True
            profile = (runtime_root() / 'config-sync/repository-credentials'
                       / ('repository-' + hashlib.sha256(repository.encode()).hexdigest() + '.json'))
            self.credentials(repository, profile)
            applied = True
            self.approval(repository, bare, root / 'managed.txt')
            self.populated_conflicts(repository, bare, root / 'managed.txt')
        finally:
            failure_command = self.last if sys.exc_info()[0] else None
            clean = True
            if repository is None and name is not None:
                try:
                    matches = [item for item in self.call('repository', 'list', '--q', name)['repositories']
                               if item['display_name'] == name and item['external_ref'] == str(bare)]
                    require(len(matches) <= 1, 'ambiguous exact fixture repository')
                    repository = matches[0]['id'] if matches else None
                except Exception as error:
                    clean = False
                    self.cleanup_errors.append('recover repository: ' + str(error))
            if applied:
                try:
                    confirmed_command(self.pb, 'config', 'unassign', self.machine)
                    require(self.state()['state'] == 'disabled' and not self.conflicts(), 'assignment cleanup left active state')
                except Exception as error:
                    clean = False
                    self.cleanup_errors.append('unassign: ' + str(error))
            if repository and clean:
                try:
                    self.call('repository', 'credentials', 'remove', repository)
                    require(sync.api_token_request('/v1/config-repositories/' + repository, 'DELETE') == 204,
                            'repository cleanup did not confirm deletion')
                except Exception as error:
                    clean = False
                    self.cleanup_errors.append('repository cleanup: ' + str(error))
            if clean:
                sync.MACHINE_FILE.unlink(missing_ok=True)
            sync.ROOT = old_root
            if failure_command is not None:
                self.last = failure_command

    def run(self, password):
        self.error_paths()
        self.groups()
        if set(self.args.rows or ROWS) & SETTINGS:
            try:
                self.settings(password)
            except Exception as error:
                for row in SETTINGS:
                    self.rows[row]['failures'].append({'phase': 'main', 'reason': str(error), 'command': self.last})
        if set(self.args.rows or ROWS) & REPOSITORIES:
            # Retain fixture files if cleanup fails, rather than leave an active assignment pointing at deleted data.
            root = Path(tempfile.mkdtemp(prefix='pb-config-commands-'))
            try:
                self.repositories(root)
            except Exception as error:
                for row in REPOSITORIES:
                    if not self.rows[row]['main']:
                        self.rows[row]['failures'].append({'phase': 'main', 'reason': str(error), 'command': self.last})
            if not self.cleanup_errors:
                shutil.rmtree(root)
        require(registration() == self.before, 'config command check changed enrollment')
        selected = self.args.rows or ROWS
        passed = not self.cleanup_errors and all(self.rows[row]['main'] and self.rows[row]['error']
                                                and not self.rows[row]['failures'] for row in selected)
        receipt = {'check': 'config-commands', 'status': 'PASS' if passed else 'FAIL',
                   'rows': {row: self.rows[row] for row in selected}, 'cleanup_errors': self.cleanup_errors,
                   'enrollment_preserved': True}
        if self.args.output:
            with os.fdopen(os.open(self.args.output, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600), 'w') as output:
                json.dump(receipt, output)
        print(json.dumps(receipt))
        return 0 if passed else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--rows', nargs='+', choices=ROWS)
    parser.add_argument('--output')
    args = parser.parse_args()
    return Suite(args).run(sys.stdin.readline().rstrip('\n'))


if __name__ == '__main__':
    raise SystemExit(main())
