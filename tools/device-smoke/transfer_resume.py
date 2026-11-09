#!/usr/bin/env python3
"""Interrupt a large batch mid-send; every file must arrive once, byte-identical, and the same send finishes by itself.

--interrupt receiver (default): restart the receiver's daemon. sender: restart this machine's daemon.
network: drop this user's outbound non-loopback traffic for DROP_SECONDS (iptables, needs sudo -n; removed on
every exit path and by a root failsafe timer, as network_drop.py does).
"""
import argparse
import json
import os
import subprocess
import threading
import time
import uuid

from exec_lifecycle import run
from install_lifecycle import registration
import windows_target
import winhost

FILES = 4
FILE_SIZE = 24 << 20  # 96 MiB batch: about two minutes on the hp-dadape path; per-file limit is 50 MiB
SIZE = FILES * FILE_SIZE
RESTART_AFTER = 25.0
SEND_TIMEOUT = 600
DROP_SECONDS = 15
RULE = ['OUTPUT', '-m', 'owner', '--uid-owner', str(os.getuid()), '!', '-o', 'lo', '-j', 'DROP']


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--interrupt', choices=('receiver', 'sender', 'network'), default='receiver')
    args = parser.parse_args()
    binary = winhost.pb_path()
    if winhost.device() and args.interrupt == 'network':
        parser.error('--interrupt network drops traffic with iptables; it has no Windows form')
    before = registration()
    prefix = 'paperboat-smoke-resume-' + uuid.uuid4().hex
    expected = {}
    cleanup = ("from pathlib import Path\n"
               "root=Path.home()/'Paperboat/Inbox'\n"
               "for path in root.glob(" + repr(prefix + '*') + "): path.unlink()\n")
    failure = None
    with winhost.workdir('paperboat-resume-') as directory:
        sources = []
        for number in range(FILES):
            path = directory / ('%s-%d.bin' % (prefix, number))
            expected[path.name] = winhost.write_random(path, FILE_SIZE)
            sources.append(str(path))
        restarted = {}

        def interrupt():
            time.sleep(RESTART_AFTER)
            command = '(sleep 1; ~/.local/bin/pb service restart) >/dev/null 2>&1 &'
            if args.interrupt == 'receiver' and windows_target.is_windows(binary, args.target):
                run(binary, 'exec', args.target, '--', *windows_target.ps_argv(
                    windows_target.restart_service_detached_script(windows_target.target_pb(binary, args.target))))
            elif args.interrupt == 'receiver':
                run(binary, 'exec', args.target, '--', 'sh', '-c', command)
            elif args.interrupt == 'sender' and winhost.device():
                binary.run_remote(['service', 'restart'], timeout=120)
            elif args.interrupt == 'sender':
                subprocess.run(['sh', '-c', command], timeout=30, check=True)
            else:
                # Root failsafe removes the rule even if this process dies.
                subprocess.Popen(['sudo', '-n', 'sh', '-c', 'sleep %d; iptables -D %s' % (DROP_SECONDS + 60, ' '.join(RULE))],
                                 stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
                subprocess.run(['sudo', '-n', 'iptables', '-I', *RULE], timeout=30, check=True)
                try:
                    time.sleep(DROP_SECONDS)
                finally:
                    subprocess.run(['sudo', '-n', 'iptables', '-D', *RULE], timeout=30)
            restarted['at'] = time.monotonic()

        try:
            thread = threading.Thread(target=interrupt, daemon=True)
            started = time.monotonic()
            thread.start()
            result = winhost.run_pb(binary, ['send', *sources, '--to', args.target, '--json'], SEND_TIMEOUT)
            finished = time.monotonic()
            thread.join(timeout=60)
            if result.returncode:
                try:
                    failure_json = json.loads(result.stdout)['error']
                    detail = '%s %s' % (failure_json.get('code'), failure_json.get('support_reference'))
                except (ValueError, KeyError, TypeError):
                    detail = result.stderr[-200:].decode(errors='replace')
                raise RuntimeError('interrupted send did not finish by itself (exit %d, %.1fs after start): %s' % (result.returncode, finished - started, detail))
            batch = json.loads(result.stdout)
            batch = batch.get('data', batch)
            if len(batch['transfers']) != FILES:
                raise RuntimeError('resumed send did not report every file')
            if 'at' not in restarted or finished - restarted['at'] < 2 or restarted['at'] - started < RESTART_AFTER - 1:
                raise RuntimeError('the transfer was not still running when the %s interruption ended' % args.interrupt)
            verify = ("from pathlib import Path\nimport hashlib\n"
                      "root=Path.home()/'Paperboat/Inbox'\n"
                      "expected=" + repr(expected) + "\n"
                      "files=sorted(root.glob(" + repr(prefix + '*') + "))\n"
                      "assert [f.name for f in files]==sorted(expected),[f.name for f in files]\n"
                      "for f in files:\n"
                      " assert f.stat().st_size==" + str(FILE_SIZE) + "\n"
                      " h=hashlib.sha256()\n"
                      " with f.open('rb') as fh:\n"
                      "  for b in iter(lambda:fh.read(1<<20),b''): h.update(b)\n"
                      " assert h.hexdigest()==expected[f.name],f.name\n"
                      "print('PB_RESUME_VERIFIED')\n")
            windows = windows_target.is_windows(binary, args.target)
            output, errors = run(binary, 'exec', args.target, '--', *(
                windows_target.ps_argv(windows_target.inbox_verify_script(expected, 'PB_RESUME_VERIFIED', FILE_SIZE, prefix))
                if windows else ['python3', '-c', verify]))
            if output != b'PB_RESUME_VERIFIED\n' or errors:
                raise RuntimeError('received file was not verified')
        except Exception as error:
            failure = error
            raise
        finally:
            try:
                run(binary, 'exec', args.target, '--', *(
                    windows_target.ps_argv(windows_target.inbox_cleanup_glob_script(prefix))
                    if windows_target.is_windows(binary, args.target) else ['python3', '-c', cleanup]))
            except Exception as error:
                if failure is None:
                    raise
                failure.add_note('Exact received-file cleanup failed: ' + str(error))
    if registration() != before:
        raise RuntimeError('transfer resume changed the enrolled identity')
    print(json.dumps({'check': 'transfer-resume', 'target': args.target, 'interrupt': args.interrupt, 'status': 'PASS',
                      'bytes': SIZE, 'interrupted_after_s': RESTART_AFTER, 'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
