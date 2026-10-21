#!/usr/bin/env python3
"""Exercise configuration sync on the enrolled device with a retained local Git repository."""
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile

from install_lifecycle import registration
from pairing_lifecycle import command_result

PB = Path.home() / '.local/bin/pb'
# A local repository exists only at its creator's path, so every machine registers its own.
FIXTURE_NAME = 'smoke-config-fixture-' + socket.gethostname()


def run(*args, expect_ok=True):
    result = command_result(PB, *args, '--json', timeout=120)
    try:
        value = json.loads(result.stdout)
    except ValueError:
        raise RuntimeError('command did not return JSON: ' + ' '.join(args)) from None
    if expect_ok and (result.returncode or value.get('ok') is False):
        error = value.get('error', {})
        raise RuntimeError('command failed: ' + ' '.join(args) + ' -> ' + str(error.get('code')) + ': ' + str(error.get('message')))
    return value.get('data', value)


def confirmed(*args):
    """Run a command that previews first and applies only with its confirmation code."""
    preview = run(*args, expect_ok=False)
    token = preview.get('confirmation_token')
    assert token, 'no confirmation preview for ' + ' '.join(args)
    return run(*args, '--confirm', token)


def fixture_repository():
    """One retained local repository is reused; the CLI has no repository delete."""
    root = Path.home() / '.cache/paperboat-smoke'
    bare = root / 'config-fixture.git'
    if not bare.exists():
        root.mkdir(parents=True, exist_ok=True)
        subprocess.run(['git', 'init', '-q', '--bare', '-b', 'main', str(bare)], check=True)
        with tempfile.TemporaryDirectory() as work:
            subprocess.run(['git', 'clone', '-q', str(bare), work], check=True, capture_output=True)
            Path(work, 'README').write_text('# Paperboat smoke config fixture\n')
            env = dict(os.environ, GIT_AUTHOR_NAME='smoke', GIT_AUTHOR_EMAIL='smoke@example.test',
                       GIT_COMMITTER_NAME='smoke', GIT_COMMITTER_EMAIL='smoke@example.test')
            subprocess.run(['git', '-C', work, 'checkout', '-q', '-B', 'main'], check=True, env=env)
            subprocess.run(['git', '-C', work, 'add', '-A'], check=True, env=env)
            subprocess.run(['git', '-C', work, 'commit', '-q', '-m', 'fixture'], check=True, env=env)
            subprocess.run(['git', '-C', work, 'push', '-q', 'origin', 'main'], check=True, env=env)
    return bare


def main():
    before = registration()
    bare = fixture_repository()
    machine_file = Path.home() / '.config/paperboat/config-sync.toml'
    backup = machine_file.read_text() if machine_file.exists() else None
    local = Path.home() / '.cache/paperboat-smoke/config-sync-aliases'
    local.write_text("alias smoke='echo smoke'\n")
    assigned = False
    primary_error = None
    try:
        listed = run('config', 'repository', 'list')
        items = listed if isinstance(listed, list) else listed.get('items', listed.get('repositories', []))
        repository = next((r for r in items if r.get('display_name') == FIXTURE_NAME
                           or r.get('name') == FIXTURE_NAME), None)
        if repository is None:
            connected = run('config', 'repository', 'connect', str(bare), '--name', FIXTURE_NAME)
            repository = connected['repository']
        repository_id = repository['id']
        assert FIXTURE_NAME in json.dumps(run('config', 'repository', 'list'))
        assert run('config', 'sync', 'path')
        machine_file.parent.mkdir(parents=True, exist_ok=True)
        machine_file.write_text(f'''version = 1
enabled = true
mode = "bidirectional"
[pull]
repository_id = "{repository_id}"
[push]
repository_id = "{repository_id}"
[paths.aliases]
local_path = "{local}"
kind = "file"
''')
        projection = run('config', 'sync', 'validate')
        assert projection['pull_repository_id'] == repository_id and projection['enabled']
        assert projection['mode'] == 'bidirectional'
        assert [r['id'] for r in projection['path_rules']] == ['machine:aliases']
        applied = confirmed('config', 'sync', 'apply')
        assigned = True
        assert applied['revision'] == projection['revision']
        status = run('config', 'status')
        mine = next(e for e in status['environments'] if e['machine_id'] == before['machine_id'])
        assert mine['mode'] == 'bidirectional' and mine['assignment_version'] >= 1
        forced = confirmed('config', 'force', 'pull', mine['alias'])
        assert forced['outcome'] == 'queued' and forced['operation']['action'] == 'force_pull'
        # The list is account-wide: only this computer's conflicts belong to this check.
        assert [c for c in run('config', 'conflict', 'list')['conflicts']
                if c.get('environment_id') == before['machine_id']] == []
        removed = confirmed('config', 'unassign', mine['alias'])
        assert removed['state'] == 'unassigned'
        assigned = False
        status = run('config', 'status')
        mine = next(e for e in status['environments'] if e['machine_id'] == before['machine_id'])
        assert mine['state'] == 'disabled'
    except BaseException as error:
        primary_error = error
        raise
    finally:
        try:
            if assigned:
                status = run('config', 'status')
                mine = next(e for e in status['environments'] if e['machine_id'] == before['machine_id'])
                confirmed('config', 'unassign', mine['alias'])
            if backup is None:
                machine_file.unlink(missing_ok=True)
            else:
                machine_file.write_text(backup)
            local.unlink(missing_ok=True)
        except BaseException:
            if primary_error is None:
                raise
            print(json.dumps({'check': 'config-sync', 'cleanup': 'failed'}), flush=True)
    assert registration() == before
    print(json.dumps({'check': 'config-sync', 'status': 'PASS', 'repository_connect_list': True,
                      'validate_apply': True, 'status_assignment': True, 'force_pull': True,
                      'unassign': True, 'enrollment_preserved': True}))


if __name__ == '__main__':
    main()
