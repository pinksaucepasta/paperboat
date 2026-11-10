#!/usr/bin/env python3
"""Qualify F1-01/S3-02 using the server-returned published enrollment command."""
import os, json, shutil, tempfile, time, hashlib, shlex
from pathlib import Path
from urllib.parse import urlparse, parse_qs
import pairing_lifecycle as p
import winhost
import terminal_command
import argparse, sys

def main():
    parser = argparse.ArgumentParser(description='Verify the published one-liner and restore the installed candidate.')
    parser.add_argument('--expect-release', required=True)
    parser.add_argument('--keep-official', action='store_true', help='leave the official install in place instead of restoring the staged custom build')
    args = parser.parse_args()
    binary = winhost.pb_path()
    password = None if winhost.device() else sys.stdin.readline().rstrip('\n')  # Windows asks through UAC, not sudo
    if not password and not winhost.device():
        raise RuntimeError('sudo credential required on stdin')
    root = p.runtime_root()
    before = json.loads((root / 'machine-registration.json').read_text())
    machines = p.invoke(binary, '--workspace', 'personal', 'machine', 'list', '--all', '--json')['machines']
    original = next((x for x in machines if x['id'] == before['machine_id']))
    directory, staged, command, tokenfile = p.published_fixture(binary, original['alias'])
    inbox = winhost.device_path(before['inbox_path'])
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
        p.invoke(staged, 'uninstall', '--confirmation', 'UNINSTALL PAPERBOAT', '--hostname', p.hostname(binary), '--json', raw=True, terminal=True, password=password)
        if winhost.device():  # the removal finishes after the command returns
            p.inventory(None, inbox, installed=False, password=None)
        p.run_published(binary, command, directory, password)
        version = p.invoke(binary, '--version', raw=True)
        assert args.expect_release in version, 'published version mismatch'
        after = json.loads((root / 'machine-registration.json').read_text())
        assert after['account_id'] == before['account_id'], 'account changed'
        current = p.invoke(binary, '--workspace', 'personal', 'machine', 'list', '--all', '--json')['machines']
        assert next((x for x in current if x['id'] == after['machine_id']))['alias'] == original['alias'], 'alias changed'
        assert not any((x['id'] == before['machine_id'] for x in current)), 'disposed record remains'
        deadline = time.monotonic() + 120  # the new runtime needs a moment after install
        while p.invoke(binary, 'doctor', '--json', timeout=60)['overall'] != 'healthy':
            assert time.monotonic() < deadline, 'official doctor unhealthy'
            time.sleep(5)
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
            p.remove_tree(directory)
    if error:
        print(json.dumps({'check': 'published-install', 'status': 'FAIL', 'error': str(error), 'recovery_directory': str(directory)}), flush=True)
        raise SystemExit(1)
    print(json.dumps({'check': 'published-install', 'status': 'PASS', 'version': args.expect_release, 'seconds': round(time.monotonic() - started, 1), 'account_alias_inbox_preserved': True, 'custom_build_restored': not args.keep_official}), flush=True)


if __name__ == "__main__":
    main()
