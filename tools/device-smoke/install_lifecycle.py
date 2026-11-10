#!/usr/bin/env python3
"""Run bounded installation checks as the enrolled user; emit no identities.

With PB_SMOKE_WINDOWS_PB set the checks drive pb.exe on the Windows device (winhost.py); the service supervisor
there is the Windows service manager and the boot identity is the last boot time."""
import argparse
import fcntl
import signal
import shlex
import json
import os
import re
from pathlib import Path
import subprocess
import sys
import time

import winhost


def registration():
    relative = ('.local/state/paperboat/runtime' if os.uname().sysname == 'Linux'
                else 'Library/Application Support/Paperboat/runtime')
    root = winhost.runtime_dir(Path.home() / relative)
    record = json.loads((root / 'machine-registration.json').read_text())
    return {key: record[key] for key in ('machine_id', 'environment_id',
            'public_identity_key', 'public_key_id', 'installation_generation', 'account_id', 'inbox_path')}


# Windows forms of the supervisor queries.
SERVICE_PID = ("(Get-CimInstance Win32_Service|Where-Object{$_.Name -like 'PaperboatLocalDaemon*'}|"
               "Select-Object -First 1).ProcessId")
BOOT_TIME = "(Get-CimInstance Win32_OperatingSystem).LastBootUpTime.ToUniversalTime().ToString('o')"


INSTALLER_RUNNING = "[bool](Get-Process -Name '.pb-elevated-*' -ErrorAction SilentlyContinue)"


def system_path(device):
    root = device.env('SystemRoot')
    return ';'.join(root + tail for tail in ('\\System32', '', '\\System32\\Wbem', '\\System32\\WindowsPowerShell\\v1.0'))


def command(pb, *args, timeout=45, env=None):
    from pairing_lifecycle import command_result
    result = command_result(pb, *args, timeout=timeout, env=env, input='')
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
    parser.add_argument('check', choices=['reinstall', 'pair-refusal', 'service', 'interrupt-install', 'crash-service', 'shell-install', 'post-reboot', 'setup'])
    parser.add_argument('--sudo-password-stdin', action='store_true')
    parser.add_argument('--shell')
    parser.add_argument('--previous-boot-id')
    args = parser.parse_args()
    password = sys.stdin.readline().rstrip('\n') if args.sudo_password_stdin else None
    pb = winhost.pb_path()
    device = winhost.device()
    before = registration()
    started = time.monotonic()
    if args.check == 'setup':
        from pairing_lifecycle import invoke
        machines = invoke(pb, 'machine', 'list', '--all', '--json')['machines']  # the default page is 50 machines
        machine = next(item for item in machines if item['id'] == before['machine_id'])
        invoke(pb, 'setup', '--name', machine['alias'], raw=True,
               terminal=True, password=password)
        command(pb, 'doctor', '--json')
        invoke(pb, 'exec', machine['alias'], '--', *winhost.true_argv(), raw=True, timeout=45)
    elif args.check == 'post-reboot':
        boot = (device.ps('boot time', BOOT_TIME)[1] if device
                else Path('/proc/sys/kernel/random/boot_id').read_text().strip() if os.uname().sysname == 'Linux'
                else subprocess.run(['sysctl', '-n', 'kern.boottime'], capture_output=True, text=True, check=True).stdout.strip())
        if not args.previous_boot_id or boot == args.previous_boot_id:
            raise RuntimeError('device did not reboot since the saved observation')
        command(pb, 'doctor', '--json')
        # A healthy local doctor is not enough: the server must see the runtime again (macOS once stayed offline).
        deadline = time.monotonic() + 180
        while True:
            from pairing_lifecycle import invoke
            machines = invoke(pb, 'machine', 'list', '--all', '--json')['machines']  # the default page is 50 machines
            if any(m['id'] == before['machine_id'] and m.get('online') for m in machines):
                break
            if time.monotonic() > deadline:
                raise RuntimeError('machine did not come back online in the server inventory after the reboot')
            time.sleep(5)
    elif args.check == 'shell-install':
        if device:  # PowerShell 7 as the user's shell, with only the system's own PATH
            import pathlib
            if not args.shell or not pathlib.PureWindowsPath(args.shell).is_absolute():
                raise RuntimeError('shell must be an absolute installed executable')
            result = device.run([args.shell, '-NoLogo', '-Command', '& %s install --json' % winhost.ps_quote(pb)],
                                env={'PATH': system_path(device)}, timeout=180)
            receipt = json.loads(result.stdout)
            if result.returncode or receipt.get('ok') is not True:
                raise RuntimeError('install from the shell failed; output withheld')
        elif not args.shell or not Path(args.shell).is_absolute():
            raise RuntimeError('shell must be an absolute installed executable')
        previous = os.umask(0o077)
        try:
            result = None if device else subprocess.run([args.shell, '-l', '-c', shlex.quote(str(pb)) +
                                     ' install --json'], capture_output=True, text=True,
                                    stdin=subprocess.DEVNULL, timeout=180,
                                    env=dict(os.environ, SHELL=args.shell,
                                             PATH='/usr/bin:/bin:/usr/sbin:/sbin'))
        finally:
            os.umask(previous)
        if not device:
            receipt = json.loads(result.stdout)
            if result.returncode or receipt.get('ok') is not True:
                raise RuntimeError('install from login shell failed; output withheld')
    elif args.check == 'crash-service':
        def service_pid():
            if device:
                return int(device.ps('daemon service pid', SERVICE_PID)[1] or '0')
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
            device.stop(previous_pid) if device else os.kill(previous_pid, signal.SIGKILL)
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
    elif args.check == 'interrupt-install' and device:
        try:  # Ctrl-C through pb's console while its elevated installer is at work
            fired, code = device.interrupt_when([str(pb), 'install', '--json'], INSTALLER_RUNNING, seconds=15)
            if not fired or code == 0:
                raise RuntimeError('did not interrupt a running installation transaction')
        finally:
            command(pb, 'install', '--json', timeout=180)
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
            # The system's own PATH only: the install must not depend on pb's directory being on it.
            receipt = command(pb, 'install', '--json', timeout=600 if device else 180,  # a slow hosted runner took 194 s once
                              env={'PATH': system_path(device)} if device
                              else dict(os.environ, PATH='/usr/bin:/bin:/usr/sbin:/sbin'))
        finally:
            os.umask(previous)
        if receipt.get('enrollment') != 'unchanged':
            raise RuntimeError('install did not confirm preserved enrollment')
        if receipt.get('automatic_update_checks') is not False:
            raise RuntimeError('custom build enabled automatic updates')
    elif args.check == 'pair-refusal':
        from pairing_lifecycle import command_result
        result = command_result(pb, 'pair', '--json', timeout=45, input='')
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
    result = {'check': args.check, 'status': 'PASS',
              'seconds': round(time.monotonic() - started, 1),
              'enrollment_preserved': True}
    if args.check == 'setup':
        result['native_exec'] = True
    print(json.dumps(result))


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print(json.dumps({'status': 'FAIL', 'reason': str(error)}))
        raise SystemExit(1)
