#!/usr/bin/env python3
"""Qualify F1-01/S3-02 using the server-returned published enrollment command."""
import os, json, shutil, tempfile, time, hashlib, shlex
from pathlib import Path
from urllib.parse import urlparse, parse_qs
import pairing_lifecycle as p
import terminal_command
import argparse, sys

def main():
    parser = argparse.ArgumentParser(description='Verify the published one-liner and restore the installed candidate.')
    parser.add_argument('--expect-release', required=True)
    parser.add_argument('--keep-official', action='store_true', help='leave the official install in place instead of restoring the staged custom build')
    args = parser.parse_args()
    password = sys.stdin.readline().rstrip('\n')
    if not password:
        raise RuntimeError('sudo credential required on stdin')
    binary = Path.home() / '.local/bin/pb'
    root = p.runtime_root()
    before = json.loads((root / 'machine-registration.json').read_text())
    machines = p.invoke(binary, '--workspace', 'personal', 'machine', 'list', '--json')['machines']
    original = next((x for x in machines if x['id'] == before['machine_id']))
    directory = Path(tempfile.mkdtemp(prefix='pb-published-proof-'))
    staged = directory / 'pb'
    shutil.copyfile(binary, staged)
    staged.chmod(0o700)
    receipt = p.invoke(binary, '--workspace', 'personal', 'machine', 'add', '--name', original['alias'], '--json')
    command = receipt['linux_macos_command']
    address = next((w for w in shlex.split(command) if w.startswith('https://')))
    token = parse_qs(urlparse(address).query)['p'][0][-26:]
    assert len(token) == 26 and token.isalnum() and (token.upper() == token)
    tokenfile = directory / 'enrollment-token'
    tokenfile.touch(mode=0o600)
    tokenfile.write_text(token)
    inbox = Path(before['inbox_path'])
    sentinel = inbox / ('.pb-install-proof-' + directory.name)
    sentinel.write_bytes(os.urandom(64))
    sentinel.chmod(0o600)
    digest = hashlib.sha256(sentinel.read_bytes()).digest()
    started = time.monotonic()
    error = None
    removed = False
    official = False
    try:
        p.invoke(staged, '--workspace', 'personal', '__desktop', 'request', input=json.dumps({'action': 'machine.remove', 'payload': {'machine_id': before['machine_id']}}))
        removed = True
        p.invoke(staged, 'uninstall', '--confirmation', 'UNINSTALL PAPERBOAT', '--hostname', os.uname().nodename, '--json', raw=True, terminal=True, password=password)
        result = terminal_command.run(['/bin/sh', '-c', command], password=password, timeout=600)
        if result.returncode:
            diagnostic = p.command_diagnostic(['/bin/sh', '-c', command], result.stdout, result.stderr)
            raise RuntimeError('published installer failed; private diagnostics: ' + str(diagnostic))
        version = p.invoke(binary, '--version', raw=True)
        assert args.expect_release in version, 'published version mismatch'
        after = json.loads((root / 'machine-registration.json').read_text())
        assert after['account_id'] == before['account_id'], 'account changed'
        current = p.invoke(binary, '--workspace', 'personal', 'machine', 'list', '--json')['machines']
        assert next((x for x in current if x['id'] == after['machine_id']))['alias'] == original['alias'], 'alias changed'
        assert not any((x['id'] == before['machine_id'] for x in current)), 'disposed record remains'
        assert p.invoke(binary, 'doctor', '--json', timeout=60)['overall'] == 'healthy', 'official doctor unhealthy'
        assert hashlib.sha256(sentinel.read_bytes()).digest() == digest, 'Inbox changed'
        official = True
    except Exception as exc:
        error = exc
    finally:
        if removed and not (args.keep_official and official):
            try:
                p.invoke(staged, 'install', '--json', raw=True, terminal=True, password=password, timeout=300)
                if not official:
                    p.pair(staged, ['pair', '--server', before['server_url'], '--enrollment-token-file', str(tokenfile), '--name', original['alias']], root, 'none', before['server_url'], password)
                p.invoke(binary, 'inbox', 'set', str(inbox), '--json')
                assert p.invoke(binary, 'doctor', '--json', timeout=60)['overall'] == 'healthy', 'restored doctor unhealthy'
            except Exception as exc:
                print(json.dumps({'check': 'published-install', 'status': 'FAIL', 'restore_error': str(exc), 'error': str(error), 'recovery_directory': str(directory)}), flush=True)
                raise SystemExit(1)
        sentinel.unlink(missing_ok=True)
        if error is None:
            shutil.rmtree(directory)
    if error:
        print(json.dumps({'check': 'published-install', 'status': 'FAIL', 'error': str(error), 'recovery_directory': str(directory)}), flush=True)
        raise SystemExit(1)
    print(json.dumps({'check': 'published-install', 'status': 'PASS', 'version': args.expect_release, 'seconds': round(time.monotonic() - started, 1), 'account_alias_inbox_preserved': True, 'custom_build_restored': not args.keep_official}), flush=True)


if __name__ == "__main__":
    main()
