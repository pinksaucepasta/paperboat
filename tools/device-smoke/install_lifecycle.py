#!/usr/bin/env python3
"""Run bounded installation checks as the enrolled Unix user; emit no identities."""
import argparse
import fcntl
import signal
import shlex
import json
import os
import re
from pathlib import Path
import subprocess
import time


def registration():
    relative = ('.local/state/paperboat/runtime' if os.uname().sysname == 'Linux'
                else 'Library/Application Support/Paperboat/runtime')
    record = json.loads((Path.home() / relative / 'machine-registration.json').read_text())
    return {key: record[key] for key in ('machine_id', 'environment_id',
            'public_identity_key', 'public_key_id', 'installation_generation', 'account_id')}


def command(pb, *args, timeout=45, env=None):
    result = subprocess.run([str(pb), *args], capture_output=True, text=True,
                            timeout=timeout, env=env, stdin=subprocess.DEVNULL)
    try:
        receipt = json.loads(result.stdout)
    except ValueError:
        raise RuntimeError('command did not return a JSON receipt') from None
    if args == ('doctor', '--json'):
        if result.returncode or receipt.get('overall') != 'healthy':
            raise RuntimeError('doctor did not report healthy after daemon recovery')
        return receipt
    if result.returncode or receipt.get('ok') is not True:
        raise RuntimeError(' '.join(args[:2]) + ' failed; code=' +
                           str(receipt.get('error', {}).get('code', 'unknown')))
    return receipt.get('data', {})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('check', choices=['reinstall', 'pair-refusal', 'service', 'interrupt-install', 'crash-service', 'shell-install', 'post-reboot'])
    parser.add_argument('--shell')
    parser.add_argument('--previous-boot-id')
    args = parser.parse_args()
    pb = Path.home() / '.local/bin/pb'
    before = registration()
    started = time.monotonic()
    if args.check == 'post-reboot':
        boot = Path('/proc/sys/kernel/random/boot_id').read_text().strip()
        if not args.previous_boot_id or boot == args.previous_boot_id:
            raise RuntimeError('device did not reboot since the saved observation')
        command(pb, 'doctor', '--json')
    elif args.check == 'shell-install':
        if not args.shell or not Path(args.shell).is_absolute():
            raise RuntimeError('shell must be an absolute installed executable')
        previous = os.umask(0o077)
        try:
            result = subprocess.run([args.shell, '-l', '-c', shlex.quote(str(pb)) +
                                     ' install --json'], capture_output=True, text=True,
                                    stdin=subprocess.DEVNULL, timeout=180,
                                    env=dict(os.environ, SHELL=args.shell,
                                             PATH='/usr/bin:/bin:/usr/sbin:/sbin'))
        finally:
            os.umask(previous)
        receipt = json.loads(result.stdout)
        if result.returncode or receipt.get('ok') is not True:
            raise RuntimeError('install from login shell failed; output withheld')
    elif args.check == 'crash-service':
        def service_pid():
            if os.uname().sysname == 'Linux':
                query = ['systemctl', '--user', 'show', 'paperboatd.service',
                         '--property=MainPID', '--value']
                result = subprocess.run(query, capture_output=True, text=True, timeout=5)
                return int(result.stdout.strip() or '0')
            query = ['launchctl', 'print', 'gui/' + str(os.getuid()) +
                     '/com.pinksaucepasta.paperboatd']
            result = subprocess.run(query, capture_output=True, text=True, timeout=5)
            match = re.search(r'^\s*pid = (\d+)$', result.stdout, re.MULTILINE)
            return int(match.group(1)) if match else 0
        previous_pid = service_pid()
        if previous_pid < 2:
            raise RuntimeError('could not identify the supervised daemon')
        try:
            os.kill(previous_pid, signal.SIGKILL)
            deadline = time.monotonic() + 30
            while time.monotonic() < deadline:
                current = service_pid()
                if current > 1 and current != previous_pid:
                    try:
                        command(pb, 'doctor', '--json')
                        break
                    except RuntimeError:
                        # A new OS process is not yet an initialized API.
                        # Require healthy recovery within the SAME30-second bound.
                        pass
                time.sleep(0.2)
            else:
                raise RuntimeError('supervisor did not restore the crashed daemon')
        finally:
            command(pb, 'service', 'start', '--json')
    elif args.check == 'interrupt-install':
        state = Path.home() / ('.local/state/paperboat' if os.uname().sysname == 'Linux'
                               else 'Library/Application Support/Paperboat/state')
        lock = state / 'install.lock'
        process = subprocess.Popen([str(pb), 'install', '--json'], stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, stdin=subprocess.DEVNULL,
                                   start_new_session=True)
        interrupted = False
        try:
            deadline = time.monotonic() + 15
            while process.poll() is None and time.monotonic() < deadline:
                if lock.exists():
                    with lock.open('rb') as handle:
                        try:
                            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
                            fcntl.flock(handle, fcntl.LOCK_UN)
                        except BlockingIOError:
                            os.killpg(process.pid, signal.SIGINT)
                            interrupted = True
                            break
                time.sleep(0.01)
            process.communicate(timeout=90)
            if not interrupted or process.returncode == 0:
                raise RuntimeError('did not interrupt a running installation transaction')
        finally:
            if process.poll() is None:
                process.kill()
                process.communicate(timeout=10)
            # Retry the exact same command, without deleting locks or journals.
            command(pb, 'install', '--json', timeout=180)
    elif args.check == 'reinstall':
        previous = os.umask(0o077)
        try:
            receipt = command(pb, 'install', '--json', timeout=180,
                              env=dict(os.environ, PATH='/usr/bin:/bin:/usr/sbin:/sbin'))
        finally:
            os.umask(previous)
        if receipt.get('enrollment') != 'unchanged':
            raise RuntimeError('install did not confirm preserved enrollment')
        if receipt.get('automatic_update_checks') is not False:
            raise RuntimeError('custom build enabled automatic updates')
    elif args.check == 'pair-refusal':
        result = subprocess.run([str(pb), 'pair', '--json'], capture_output=True,
                                text=True, timeout=45, stdin=subprocess.DEVNULL)
        receipt = json.loads(result.stdout)
        message = receipt.get('error', {}).get('message', '')
        if result.returncode == 0 or 'pb setup --name <machine-alias>' not in message:
            raise RuntimeError('existing enrollment refusal lost its recovery command')
    else:
        try:
            for operation in ['install', 'stop', 'start', 'restart', 'uninstall', 'install']:
                receipt = command(pb, 'service', operation, '--json')
                if receipt.get('completed') is not True:
                    raise RuntimeError('service operation was not acknowledged')
                state = command(pb, 'service', 'status', '--json').get('status')
                expected = 'running' if operation in ('install', 'start', 'restart') else None
                if expected and state != expected or not expected and state == 'running':
                    raise RuntimeError('service status does not match completed operation')
        finally:
            # Leave a working daemon even when an intermediate assertion fails.
            command(pb, 'service', 'install', '--json')
    if registration() != before:
        raise RuntimeError('operation changed the protected enrollment')
    if command(pb, 'service', 'status', '--json').get('status') != 'running':
        raise RuntimeError('local daemon was not left running')
    print(json.dumps({'check': args.check, 'status': 'PASS',
                      'seconds': round(time.monotonic() - started, 1),
                      'enrollment_preserved': True}))


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print(json.dumps({'status': 'FAIL', 'reason': str(error)}))
        raise SystemExit(1)
