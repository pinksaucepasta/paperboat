#!/usr/bin/env python3
"""F14-05/08/09 on an official install: check/download/status/bundle/tamper/offline reinstall, then restore the custom build.

Uses the published one-liner (as published_install.py) so the runtime has official provenance."""
import re, os, json, shutil, tempfile, time, hashlib, shlex
from pathlib import Path
from urllib.parse import urlparse, parse_qs
import pairing_lifecycle as p
import winhost
import terminal_command
import argparse, sys


TAMPER = ("Copy-Item -LiteralPath $req.source -Destination $req.target -Force\n"
          "$f=[IO.File]::Open($req.target,'Open','ReadWrite');$f.Position=[long]($f.Length/2);$b=$f.ReadByte()\n"
          "$f.Position=$f.Position-1;$f.WriteByte($b -bxor 0xFF);$f.Close()")


def update_checks(binary, password, directory):
    def result(*args, timeout=300):
        return p.command_result(binary, *args, timeout=timeout, terminal=True, password=password)

    def version():
        return p.invoke(binary, '--version', raw=True)

    official = version()
    status = p.invoke(binary, 'update', 'status', '--json')
    assert status['cli_version'] and status['update_available'] is False, 'update status inaccurate'
    check = result('update', 'check', '--json')
    receipt = json.loads(check.stdout[check.stdout.index('{'):])
    assert check.returncode == 0 and receipt['ok'] is True, 'update check refused on an official install'
    download = result('update', 'download', '--json')
    assert download.returncode == 0, 'update download failed on an official install: ' + re.sub(r'\x1b\[[0-9;?]*[a-zA-Z]', '', download.stdout)[-400:]
    settings = p.invoke(binary, 'update', 'settings', '--json')
    assert 'automatic_updates' in settings, 'update settings missing'
    bundle = directory / 'official.pb-update'
    p.invoke(binary, 'update', 'bundle', '--output', str(bundle), raw=True, timeout=300)
    tampered = directory / 'tampered.pb-update'
    if winhost.device():  # the bundle is too large to carry over SSH: one byte is flipped in a copy on the device
        assert bundle.size() > 1000, 'bundle is empty'
        winhost.device().ps('tamper with the bundle', TAMPER, timeout=300, source=str(bundle), target=str(tampered))
    else:
        assert bundle.stat().st_size > 1000, 'bundle is empty'
        raw = bytearray(bundle.read_bytes())
        raw[len(raw) // 2] ^= 0xFF
        tampered.write_bytes(raw)
    bad = result('update', '--from', str(tampered), '--json')
    assert bad.returncode != 0, 'tampered bundle was accepted'
    assert version() == official and p.invoke(binary, 'doctor', '--json', timeout=60)['overall'] == 'healthy', 'tampered bundle disturbed the install'
    good = result('update', '--from', str(bundle), '--json')
    assert good.returncode == 0, 'offline bundle install failed rc=%s: %s' % (good.returncode, re.sub(r'\x1b\[[0-9;?]*[a-zA-Z]', '', good.stdout)[-700:])
    assert p.invoke(binary, 'doctor', '--json', timeout=60)['overall'] == 'healthy', 'doctor unhealthy after bundle install'
    return True


def main():
    parser = argparse.ArgumentParser(description='Verify the published one-liner and restore the installed candidate.')
    parser.add_argument('--expect-release', required=True)
    args = parser.parse_args()
    binary = winhost.pb_path()
    password = None if winhost.device() else sys.stdin.readline().rstrip('\n')  # Windows asks through UAC, not sudo
    if not password and not winhost.device():
        raise RuntimeError('sudo credential required on stdin')
    root = p.runtime_root()
    before = json.loads((root / 'machine-registration.json').read_text())
    machines = p.invoke(binary, '--workspace', 'personal', 'machine', 'list', '--json')['machines']
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
        p.invoke(staged, 'uninstall', '--confirmation', 'UNINSTALL PAPERBOAT', '--hostname', p.hostname(binary), '--json')
        if winhost.device():  # the removal finishes after the command returns
            p.inventory(None, inbox, installed=False, password=None)
        p.run_published(binary, command, directory, password)
        version = p.invoke(binary, '--version', raw=True)
        assert args.expect_release in version, 'published version mismatch'
        after = json.loads((root / 'machine-registration.json').read_text())
        assert after['account_id'] == before['account_id'], 'account changed'
        current = p.invoke(binary, '--workspace', 'personal', 'machine', 'list', '--json')['machines']
        assert next((x for x in current if x['id'] == after['machine_id']))['alias'] == original['alias'], 'alias changed'
        assert not any((x['id'] == before['machine_id'] for x in current)), 'disposed record remains'
        assert p.invoke(binary, 'doctor', '--json', timeout=60)['overall'] == 'healthy', 'official doctor unhealthy'
        assert hashlib.sha256(sentinel.read_bytes()).digest() == digest, 'Inbox changed'
        updates = update_checks(binary, password, directory)
        official = True
    except Exception as exc:
        error = exc
    finally:
        if removed:
            try:
                p.invoke(staged, 'install', '--json', raw=True, terminal=True, password=password, timeout=300)
                if not official and not (root / 'machine-registration.json').exists():
                    p.pair(staged, ['pair', '--server', before['server_url'], '--enrollment-token-file', str(tokenfile), '--name', original['alias']], root, 'none', before['server_url'], password)
                p.invoke(binary, 'inbox', 'set', str(inbox), '--json')
                assert p.invoke(binary, 'doctor', '--json', timeout=60)['overall'] == 'healthy', 'restored doctor unhealthy'
            except Exception as exc:
                print(json.dumps({'check': 'official-update', 'status': 'FAIL', 'restore_error': str(exc), 'error': str(error), 'recovery_directory': str(directory)}), flush=True)
                raise SystemExit(1)
        sentinel.unlink(missing_ok=True)
        if error is None:
            p.remove_tree(directory)
    if error:
        print(json.dumps({'check': 'official-update', 'status': 'FAIL', 'error': str(error), 'recovery_directory': str(directory)}), flush=True)
        raise SystemExit(1)
    print(json.dumps({'check': 'official-update', 'status': 'PASS', 'check_download_status_bundle_tamper_offline': updates, 'version': args.expect_release, 'seconds': round(time.monotonic() - started, 1), 'account_alias_inbox_preserved': True, 'custom_build_restored': True}), flush=True)


if __name__ == "__main__":
    main()
