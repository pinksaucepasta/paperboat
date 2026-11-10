#!/usr/bin/env python3
"""Exercise complete Unix uninstall and same-command pairing recovery on devices."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import pwd
import re
import shlex
import shutil
import signal
import subprocess
import sys
import tempfile
import time
from urllib.parse import parse_qs, urlparse
import uuid

from pairing_fault import PairingFaultProxy
import terminal_command
import winhost
from failure_triage import emit_network_failure


class CommandTimeout(subprocess.TimeoutExpired):
    def __init__(self, failure, diagnostic):
        super().__init__(failure.cmd, failure.timeout, output=failure.output,
                         stderr=failure.stderr)
        self.diagnostic_path = diagnostic

    def __str__(self):
        return 'command timed out; private diagnostics: ' + str(self.diagnostic_path)


def command_diagnostic(arguments, stdout, stderr, timeout=None):
    def text(value):
        return value.decode(errors='replace') if isinstance(value, (bytes, bytearray)) else value or ''
    diagnostic = Path(tempfile.gettempdir()) / ('paperboat-f1-error-' + uuid.uuid4().hex)
    fd = os.open(diagnostic, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'w') as output:
        json.dump({'argv': arguments, 'stdout': text(stdout),
                   'stderr': text(stderr), 'timeout': timeout}, output)
    return diagnostic


class CommandFailed(RuntimeError):
    """A command exited non-zero; diagnostic_path holds its argv and output (owner-only)."""

    def __init__(self, diagnostic):
        super().__init__('command failed; private diagnostics: ' + str(diagnostic))
        self.diagnostic_path = diagnostic


def command_result(binary, *arguments, timeout=180, input=None, env=None, terminal=False, password=None):
    command = [str(binary), *arguments]
    try:
        if hasattr(binary, 'run_remote'):  # pb.exe on the Windows device; it has no pty or sudo prompt to answer
            result = binary.run_remote(arguments, input=input, env=env, timeout=timeout)
        elif terminal:
            result = terminal_command.run(command, password=password, env=env, timeout=timeout)
        else:
            result = subprocess.run(command, input=input, capture_output=True,
                                    text=True, timeout=timeout, env=env)
    except subprocess.TimeoutExpired as error:
        diagnostic = command_diagnostic(command, error.stdout, error.stderr, error.timeout)
        raise CommandTimeout(error, diagnostic) from None
    return result


def invoke(binary, *arguments, timeout=180, input=None, env=None, raw=False, terminal=False, password=None):
    command = [str(binary), *arguments]
    result = command_result(binary, *arguments, timeout=timeout, input=input, env=env,
                            terminal=terminal, password=password)
    if result.returncode:
        diagnostic = command_diagnostic(command, result.stdout, result.stderr)
        if '--json' in arguments:
            emit_network_failure(result.stdout)
        raise CommandFailed(diagnostic)
    if raw:
        return result.stdout + result.stderr
    try:
        receipt = json.loads(result.stdout)
    except ValueError:
        raise RuntimeError('command did not return JSON') from None
    if receipt.get('ok') is False:
        emit_network_failure(result.stdout)
        raise RuntimeError('command reported a JSON failure')
    return receipt.get('data', receipt)


def runtime_root():
    if winhost.device():
        return winhost.device().runtime_dir()
    if os.uname().sysname == 'Linux':
        return Path(os.environ.get('XDG_STATE_HOME', Path.home() / '.local/state')) / 'paperboat/runtime'
    return Path.home() / 'Library/Application Support/Paperboat/runtime'


def confirmed_command(binary, *arguments, timeout=45):
    preview = command_result(binary, *arguments, '--json', input='', timeout=timeout)
    receipt = json.loads(preview.stdout)
    if preview.returncode != 2 or receipt.get('error', {}).get('code') != 'confirmation_required':
        raise RuntimeError('mutation did not request its confirmation preview')
    return invoke(binary, *arguments, '--json', '--confirm',
                  receipt['data']['confirmation_token'], timeout=timeout)


def denied_exec(binary, identifier, expected_codes):
    arguments = ['exec', identifier, '--json', '--', 'true']
    denied = command_result(binary, *arguments, timeout=45)
    try:
        event = json.loads(denied.stdout)
        valid = (denied.returncode == 255 and event.get('version') == 'paperboat.exec-event/v1' and
                 event.get('event') == 'failed' and event.get('changed') is False and
                 not event.get('uncertain', False) and event.get('error_code') in expected_codes)
    except ValueError:
        valid = False
    if not valid:
        diagnostic = command_diagnostic([str(binary), *arguments], denied.stdout, denied.stderr)
        raise RuntimeError('exec was not denied before remote effect; private diagnostics: ' + str(diagnostic))
    return event


def check_machine_revoked(binary, identifier, *, perform):
    if perform:
        result = confirmed_command(binary, 'machine', 'revoke', identifier)
        if result['outcome'] != 'confirmed' or result['machine']['id'] != identifier:
            raise RuntimeError('revoke receipt did not identify the exact machine')
    machines = invoke(binary, '--workspace', 'personal', 'machine', 'list', '--all', '--json')['machines']
    # Since server 2026.10.23.48 the default machine list omits revoked and disconnected machines.
    if any(item['id'] == identifier for item in machines):
        raise RuntimeError('revoked machine remains in the server catalog')
    event = denied_exec(binary, identifier, {'machine_not_found', 'machine_resolution_failed', 'runtime_unavailable',
                        'source_identity_unavailable', 'local_transport_unavailable',
                        'transport_unavailable', 'environment_unavailable'})
    print(json.dumps({'check': 'machine-revoke', 'status': 'PASS',
                      'performed_here': perform, 'offline': True,
                      'new_exec_denied': True, 'denial_code': event['error_code']}), flush=True)


def await_external_action(directory, state, fields):
    gate = directory / 'controller-complete'
    failed = directory / 'controller-failed'
    print(json.dumps(dict(fields, state=state, gate=str(gate), failure_gate=str(failed))), flush=True)
    deadline = time.monotonic() + 300
    while not gate.exists() and not failed.exists():
        if time.monotonic() >= deadline:
            raise RuntimeError('external controller did not finish within its bound')
        time.sleep(0.1)
    if failed.exists():
        raise RuntimeError('external controller failed; restoring the owned fixture')


def await_external_revoke(directory, before, original):
    await_external_action(directory, 'external_revoke_required',
                          {'machine_id': before['machine_id'], 'account_id': before['account_id'],
                           'alias': original['alias']})


def inventory_on_device(device, inbox, installed):
    """The Windows uninstall inventory (uninstall_inventory_windows.ps1) on the device: leftovers while installed,
    none after an uninstall. The uninstall hands the removal to a helper that outlives the command, so a clean
    inventory is awaited for up to four minutes."""
    with device.temp_dir('pb-inventory') as directory:
        script = directory / 'inventory.ps1'
        script.write_text((Path(__file__).resolve().parent / 'uninstall_inventory_windows.ps1').read_text())
        deadline = time.monotonic() + 240
        while True:
            result = device.run(['powershell.exe', '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', str(script),
                                 '-Inbox', str(inbox)], timeout=300)
            clean = result.returncode == 0 and 'inventory: clean' in result.stdout
            if installed:
                if clean:
                    raise RuntimeError('inventory did not detect the installed positive control')
                return
            if clean:
                return
            if time.monotonic() > deadline:
                print(json.dumps({'check': 'uninstall-inventory', 'status': 'FAIL', 'leftovers':
                                  [line for line in result.stdout.splitlines() if line.startswith('leftover')][:40]}), flush=True)
                raise RuntimeError('uninstall inventory did not reach zero leftovers')
            time.sleep(10)


def hostname(binary):
    if hasattr(binary, 'device'):
        return binary.device.run(['hostname.exe']).stdout.strip()
    return os.uname().nodename


def published_fixture(binary, alias):
    """What published_install.py and official_update.py need before they uninstall: a kept directory, a copy of the
    installed pb in it, the account's published install command for this platform and its token in a private file."""
    receipt = invoke(binary, '--workspace', 'personal', 'machine', 'add', '--name', alias, '--json')
    if hasattr(binary, 'device'):
        directory = binary.device.home() / ('pb-published-proof-' + uuid.uuid4().hex[:12])
        directory.mkdir()
        staged = directory / 'pb.exe'
        binary.device.copy(str(binary), staged)
        staged = winhost.WindowsPb(binary.device, str(staged))
        command = receipt['windows_command']
        address = next(word.strip('\'"()') for word in command.split() if 'https://' in word)
        address = address[address.index('https://'):]
    else:
        directory = Path(tempfile.mkdtemp(prefix='pb-published-proof-'))
        staged = directory / 'pb'
        shutil.copyfile(binary, staged)
        staged.chmod(0o700)
        command = receipt['linux_macos_command']
        address = next(word for word in shlex.split(command) if word.startswith('https://'))
    token = parse_qs(urlparse(address).query)['p'][0][-26:]
    assert len(token) == 26 and token.isalnum() and token.upper() == token
    tokenfile = directory / 'enrollment-token'
    if not hasattr(binary, 'device'):
        tokenfile.touch(mode=0o600)
    tokenfile.write_text(token)
    tokenfile.chmod(0o600)
    return directory, staged, command, tokenfile


def run_published(binary, command, directory, password):
    """Run the published one-liner as a user would: /bin/sh here, a cmd.exe line on the Windows device."""
    if hasattr(binary, 'device'):
        script = directory / 'published.cmd'
        script.write_text('@' + command + '\r\n')
        try:
            result = binary.device.run(['cmd.exe', '/c', str(script)], timeout=900)
        finally:
            script.unlink()
    else:
        result = terminal_command.run(['/bin/sh', '-c', command], password=password, timeout=600)
    if result.returncode:
        diagnostic = command_diagnostic(['published install command'], result.stdout, result.stderr)
        raise RuntimeError('published installer failed; private diagnostics: ' + str(diagnostic))


def remove_tree(directory):
    directory.unlink() if isinstance(directory, winhost.RemotePath) else shutil.rmtree(directory)


def inventory(script, inbox, installed, password):
    if winhost.device():
        return inventory_on_device(winhost.device(), inbox, installed)
    sudo = ['sudo', '-n'] if password is None else ['sudo', '-S', '-p', '']
    result = subprocess.run(sudo + [sys.executable, '-I', str(script),
                             '--user', pwd.getpwuid(os.getuid()).pw_name,
                             '--inbox', str(inbox)], input=None if password is None else password + '\n',
                            capture_output=True, text=True, timeout=60)
    try:
        receipt = json.loads(result.stdout)
    except ValueError:
        raise RuntimeError('root inventory did not return a receipt') from None
    if installed:
        if result.returncode != 1 or receipt.get('status') != 'FAIL' or receipt.get('total', 0) < 1:
            raise RuntimeError('inventory did not detect the installed positive control')
    elif result.returncode or receipt.get('status') != 'PASS' or receipt.get('total') != 0:
        print(json.dumps({'check': 'uninstall-inventory', 'status': 'FAIL',
                          'leftovers': receipt.get('leftovers', {}),
                          'total': receipt.get('total'), 'file_paths': receipt.get('file_paths', []),
                          'reason': receipt.get('reason', receipt.get('error'))}), flush=True)
        raise RuntimeError('uninstall inventory did not reach zero leftovers')
    return receipt


def enrollment_file(binary, alias, directory):
    receipt = invoke(binary, '--workspace', 'personal', 'machine', 'add',
                     '--name', alias, '--json')
    if hasattr(binary, 'device'):  # tokens are per OS: the Windows enrollment, not the Linux/macOS one
        token = re.search(r"p=([^'\"&]+)", receipt['windows_command']).group(1)[-26:]
    else:
        address = next(word for word in shlex.split(receipt['linux_macos_command'])
                       if word.startswith('https://'))
        token = parse_qs(urlparse(address).query)['p'][0][-26:]
    if len(token) != 26 or any(character not in '0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ' for character in token):
        raise RuntimeError('enrollment command has invalid token shape')
    path = directory / 'enrollment-token'
    if hasattr(binary, 'device'):
        path.write_text(token)
        path.enrollment_token_file()
        return path
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'w') as output:
        output.write(token)
    return path


class FaultAbsorbed(Exception):
    """The command finished successfully despite an injected fault that really cut a connection."""

    def __init__(self, output):
        super().__init__('pairing completed despite the injected fault')
        self.output = output


def interrupt_pair_on_device(binary, arguments, root, mode, proxy):
    """interrupt_pair for pb.exe on the Windows device: Ctrl-C through pb's console, or the controller's fault proxy
    reached through an SSH forward. The rerun goes direct (the forward ends here), so no environment is returned."""
    device, journal = binary.device, root / 'bootstrap-resume.json'
    if mode == 'sigint':
        fired, code = device.interrupt_when([str(binary), *arguments], winhost.journal_when(journal, ['pairing_started']),
                                            exit_seconds=40)
        if not fired:
            raise RuntimeError('actual pairing handoff was not observed before fault injection')
        if code == 0:
            raise RuntimeError('fault did not stop pending pairing')
        return None
    with device.forward(proxy.address[1]) as port:
        address = 'http://127.0.0.1:%d' % port
        job = winhost.RemoteJob(device, [str(binary), *arguments],
                                {'HTTPS_PROXY': address, 'https_proxy': address, 'NO_PROXY': '', 'no_proxy': ''})
        try:
            deadline = time.monotonic() + 150  # each look at the device takes seconds; a Windows setup's journal appears ~54 s in
            while job.poll() is None and time.monotonic() < deadline:
                try:
                    started = json.loads(journal.read_text()).get('pairing_started')
                except (FileNotFoundError, ValueError):
                    started = False
                if started and proxy.live_count():
                    if proxy.cut() < 1:
                        raise RuntimeError('network fault did not cut an actual connection')
                    if job.wait(60) == 0:
                        raise FaultAbsorbed((job.stdout() + job.stderr()).decode(errors='replace'))
                    return None
            raise RuntimeError('actual pairing handoff was not observed before fault injection')
        finally:
            if job.poll() is None:
                job.kill()
            job.close()


def interrupt_pair(binary, arguments, root, mode, proxy):
    if hasattr(binary, 'device'):
        return interrupt_pair_on_device(binary, arguments, root, mode, proxy)
    environment = dict(os.environ)
    if proxy is not None:
        address = 'http://127.0.0.1:' + str(proxy.address[1])
        environment.update(HTTPS_PROXY=address, https_proxy=address, NO_PROXY='', no_proxy='')
    process = subprocess.Popen([str(binary), *arguments], stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE, env=environment, start_new_session=True)
    try:
        deadline = time.monotonic() + 30
        while process.poll() is None and time.monotonic() < deadline:
            try:
                journal = json.loads((root / 'bootstrap-resume.json').read_text())
            except (FileNotFoundError, ValueError):
                journal = {}
            if journal.get('pairing_started') and (proxy is None or proxy.live_count()):
                if mode == 'sigint':
                    os.kill(process.pid, signal.SIGINT)
                elif proxy.cut() < 1:
                    raise RuntimeError('network fault did not cut an actual connection')
                stdout, stderr = process.communicate(timeout=40)
                if process.returncode == 0:
                    if mode == 'network':
                        # A cut connection that the command retries through is the better outcome,
                        # as long as the cut was real (cut() >= 1 above) and the end state is checked.
                        raise FaultAbsorbed((stdout + stderr).decode(errors='replace'))
                    raise RuntimeError('fault did not stop pending pairing')
                return environment
            time.sleep(0.005)
        raise RuntimeError('actual pairing handoff was not observed before fault injection')
    finally:
        if process.poll() is None:
            process.kill()
            process.communicate(timeout=10)


def pair(binary, arguments, root, fault, server, password=None):
    proxy = None
    try:
        if fault == 'network':
            origin = urlparse(server)
            if origin.scheme != 'https':
                raise RuntimeError('network fault requires end-to-end TLS')
            proxy = PairingFaultProxy(origin.hostname, origin.port or 443)
        environment = None
        if fault != 'none':
            try:
                environment = interrupt_pair(binary, arguments, root, fault, proxy)
            except FaultAbsorbed as absorbed:
                return absorbed.output
        # Exact argv, token file and protected journal; no manual recovery edits.
        return invoke(binary, *arguments, env=environment, raw=True, terminal=True, password=password)
    finally:
        if proxy is not None:
            proxy.close()


def await_windows_uninstall(receipt, seconds=360):
    """Windows uninstall ends in a helper that finishes after the command returned (state pending): wait for its status.json
    to read completed before anything is installed over it (an install that raced the helper died with local_io on
    winpool-9). Elsewhere the receipt has no status path and nothing waits."""
    path = (receipt.get('data') or receipt).get('completion_status_path') if isinstance(receipt, dict) else None
    if not path or not winhost.device():
        return
    status = winhost.device_path(path)
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            state = json.loads(status.read_text()).get('state')
        except (OSError, ValueError):
            state = None
        if state == 'completed':
            return
        if state in ('failed', 'timed_out'):
            raise RuntimeError('the Windows uninstall helper ended ' + state)
        time.sleep(3)
    raise RuntimeError('the Windows uninstall helper did not complete in %d s' % seconds)


def uninstall(binary, identifier, check, inbox, script, password):
    # This is disposal of this check's exact enrolled resource, not a claim that
    # reset works. Mint restoration authority before deletion revokes its CLI.
    request = json.dumps({'action': 'machine.remove', 'payload': {'machine_id': identifier}})
    invoke(binary, '--workspace', 'personal', '__desktop', 'request', input=request)
    if check == 'broken':
        invoke(binary, 'service', 'uninstall', '--json')
    if check == 'prompts':
        invoke(binary, 'uninstall', input='UNINSTALL PAPERBOAT\n' + hostname(binary) + '\n', raw=True)
    else:
        receipt = invoke(binary, 'uninstall', '--confirmation', 'UNINSTALL PAPERBOAT',
                         '--hostname', hostname(binary), '--json')
        await_windows_uninstall(receipt)
    inventory(script, inbox, installed=False, password=password)


def qualify(expected, check, fault, script, password, revoke_first=False, external_revoke=False):
    binary = winhost.pb_path()
    version = command_result(binary, '--version', timeout=30)
    if version.returncode or expected not in version.stdout:
        raise RuntimeError('installed executable does not match the exact candidate')
    root = runtime_root()
    before = json.loads((root / 'machine-registration.json').read_text())
    machines = invoke(binary, '--workspace', 'personal', 'machine', 'list', '--all', '--json')['machines']
    original = next((item for item in machines if item['id'] == before['machine_id']), None)
    if original is None or original['owner_account'] != before['account_id'] or original['public_identity_key'] != before['public_identity_key']:
        raise RuntimeError('current account does not own the exact installed machine')
    if revoke_first or external_revoke:
        invoke(binary, 'exec', original['alias'], '--', 'true', raw=True, timeout=45)
    inbox = winhost.device_path(before['inbox_path'])
    inventory(script, inbox, installed=True, password=password)
    inbox.mkdir(parents=True, exist_ok=True)
    sentinel = inbox / ('.paperboat-f1-' + uuid.uuid4().hex)
    sentinel.write_bytes(os.urandom(64))
    sentinel.chmod(0o600)
    digest = hashlib.sha256(sentinel.read_bytes()).digest()
    started = time.monotonic()
    try:
        with winhost.workdir('pb-pair-lifecycle-') as temporary:
            staged = temporary / ('pb.exe' if winhost.device() else 'pb')
            winhost.copy_file(str(binary), staged)
            if winhost.device():
                staged = winhost.WindowsPb(winhost.device(), str(staged))
            token = enrollment_file(binary, original['alias'], temporary)
            pending_error = None
            if revoke_first or external_revoke:
                try:
                    if external_revoke:
                        await_external_revoke(temporary, before, original)
                    check_machine_revoked(staged, before['machine_id'], perform=not external_revoke)
                except Exception as error:
                    pending_error = error
            try:
                uninstall(staged, before['machine_id'], check, inbox, script, password)
            except Exception as error:
                if pending_error is None:
                    pending_error = error
                else:
                    pending_error = RuntimeError(str(pending_error) + '; uninstall also failed: ' + str(error))
            # Restore even when an inventory assertion fails, while preserving
            # that primary failure. Never call a failed uninstall test a PASS.
            invoke(staged, 'install', '--json')
            arguments = ['pair', '--server', before['server_url'],
                         '--enrollment-token-file', str(token), '--name', original['alias']]
            output = pair(staged, arguments, root, fault, before['server_url'], password)
            after = json.loads((root / 'machine-registration.json').read_text())
            machines = invoke(binary, '--workspace', 'personal', 'machine', 'list', '--all', '--json')['machines']
            current = next((item for item in machines if item['id'] == after['machine_id']), None)
            if current is None or current['alias'] != original['alias'] or after['account_id'] != before['account_id']:
                raise RuntimeError('restoration lost its exact alias or account')
            if any(item['id'] == before['machine_id'] for item in machines):
                raise RuntimeError('disposed test resource still appears active')
            if invoke(binary, 'doctor', '--json', timeout=45).get('overall') != 'healthy':
                raise RuntimeError('restored machine is not healthy')
            if hashlib.sha256(sentinel.read_bytes()).digest() != digest:
                raise RuntimeError('uninstall or restore changed the preserved Inbox fixture')
            if pending_error is not None:
                raise pending_error
            layers = invoke(binary, 'env', 'host', 'show', '--json').get('layers')
            print(json.dumps({'check': 'uninstall-pair', 'status': 'PASS',
                              'seconds': round(time.monotonic() - started, 1),
                              'mode': check, 'pair_fault': fault, 'zero_leftovers': True,
                              'inbox_preserved': True, 'alias_preserved': True,
                              'no_env_assigned': layers == [],
                              'env_pending_explained': 'managed environment variables are pending' in output}), flush=True)
    finally:
        sentinel.unlink(missing_ok=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--expect-version', required=True)
    parser.add_argument('--check', choices=['flags', 'prompts', 'broken'], required=True)
    parser.add_argument('--pair-fault', choices=['none', 'sigint', 'network'], default='none')
    parser.add_argument('--inventory', type=Path, required=True)
    parser.add_argument('--sudo-password-stdin', action='store_true')
    revoke = parser.add_mutually_exclusive_group()
    revoke.add_argument('--revoke-first', action='store_true')
    revoke.add_argument('--external-revoke', action='store_true')
    args = parser.parse_args()
    password = sys.stdin.readline().rstrip('\n') if args.sudo_password_stdin else None
    if password == '':
        raise RuntimeError('missing test-device sudo credential on stdin')
    qualify(args.expect_version, args.check, args.pair_fault, args.inventory, password,
            args.revoke_first, args.external_revoke)


if __name__ == '__main__':
    main()
