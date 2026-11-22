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
import time
import uuid

from exec_lifecycle import run
from install_lifecycle import registration
import windows_target
import winhost

FILES = 4
# The batch is sized from a pilot send so the transfer lasts about TRANSFER_SECONDS on every path (a fixed size is
# over before the interruption on a fast path and over the send timeout on a slow one). Automatic bound: 2 GB per send.
PILOT_SIZE = 32 << 20
TRANSFER_SECONDS = 60
MIN_FILE_SIZE, MAX_FILE_SIZE = 32 << 20, 400 << 20
# The interruption starts at the first progress event (`pb send --json` writes one to stderr every 5 s) and the
# send must still be running when it ends, so the check proves the transfer was in flight on every path, fast or slow.
PROGRESS_SECONDS = 120
SETTLE_SECONDS = 6  # a restart is detached (`sleep 1; pb service restart`): it has ended a few seconds after the call
SEND_TIMEOUT = 600
DROP_SECONDS = 15
RULE = ['OUTPUT', '-m', 'owner', '--uid-owner', str(os.getuid()), '!', '-o', 'lo', '-j', 'DROP']


def file_size_for(rate):
    """Bytes per batch file (whole MiB) so FILES files take about TRANSFER_SECONDS at `rate` bytes/s."""
    return max(MIN_FILE_SIZE, min(MAX_FILE_SIZE, int(rate * TRANSFER_SECONDS) // FILES >> 20 << 20))


def pilot_rate(binary, target, directory, prefix):
    """Bytes/s of one small send to the target (setup time included, so the rate is never overstated)."""
    path = directory / (prefix + '-pilot.bin')
    winhost.write_random(path, PILOT_SIZE)
    started = time.monotonic()
    result = winhost.run_pb(binary, ['send', str(path), '--to', target, '--json'], SEND_TIMEOUT)
    if result.returncode:
        raise RuntimeError('the pilot send failed (exit %d): %s' % (result.returncode, result.stderr[-200:].decode(errors='replace')))
    return PILOT_SIZE / (time.monotonic() - started)


def progress_events(stderr):
    """The transfer_progress events pb send --json wrote to stderr so far (one every 5 s, stalled or not)."""
    events = []
    for line in stderr.decode(errors='replace').splitlines():
        try:
            item = json.loads(line)
        except ValueError:
            continue
        if isinstance(item, dict) and item.get('event') == 'transfer_progress':
            events.append(item)
    return events


def in_flight(process, ended, after=0, now=time.monotonic, sleep=time.sleep):
    """Wait for a progress event past the first `after` showing 0 < bytes_sent < total; fail with `ended` if the send
    ends first. That event is the proof the batch was in flight at the moment it was written."""
    deadline = now() + PROGRESS_SECONDS
    while True:
        events = progress_events(process.stderr())
        newest = events[-1] if len(events) > after else None
        if newest and 0 < newest['bytes_sent'] < newest['total_bytes']:
            return newest
        if process.poll() is not None:
            raise RuntimeError(ended)
        if now() > deadline:
            raise RuntimeError('the send showed no progress within %ds' % PROGRESS_SECONDS)
        sleep(1)


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

    def clean_inbox():  # exactly the files this run sent
        run(binary, 'exec', args.target, '--', *(
            windows_target.ps_argv(windows_target.inbox_cleanup_glob_script(prefix))
            if windows_target.is_windows(binary, args.target) else ['python3', '-c', cleanup]))
    with winhost.workdir('paperboat-resume-') as directory:
        sources = []
        try:
            rate = pilot_rate(binary, args.target, directory, prefix)
        finally:
            clean_inbox()  # the pilot must not count as an unexpected file in the batch's verification
        file_size = file_size_for(rate)
        for number in range(FILES):
            path = directory / ('%s-%d.bin' % (prefix, number))
            expected[path.name] = winhost.write_random(path, file_size)
            sources.append(str(path))

        def interrupt():
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

        process = winhost.job(binary, ['send', *sources, '--to', args.target, '--json'])
        try:
            started = time.monotonic()
            progress = in_flight(process, 'the send ended before a progress event showed it in flight; the %d MiB fixture '
                                 'is too small for this path' % (FILES * file_size >> 20))
            interrupt()
            time.sleep(SETTLE_SECONDS)
            # Only an event written after the interruption ended proves bytes were still outstanding then.
            in_flight(process, 'the transfer was not still running when the %s interruption ended (interrupted at %d of %d '
                      'bytes)' % (args.interrupt, progress['bytes_sent'], progress['total_bytes']),
                      after=len(progress_events(process.stderr())))
            try:
                returncode = process.wait(SEND_TIMEOUT)
            except subprocess.TimeoutExpired:
                raise RuntimeError('interrupted send did not finish within %ds' % SEND_TIMEOUT) from None
            stdout = process.stdout()
            if returncode:
                try:
                    failure_json = json.loads(stdout)['error']
                    detail = '%s %s' % (failure_json.get('code'), failure_json.get('support_reference'))
                except (ValueError, KeyError, TypeError):
                    detail = process.stderr()[-200:].decode(errors='replace')
                raise RuntimeError('interrupted send did not finish by itself (exit %d, %.1fs after start): %s'
                                   % (returncode, time.monotonic() - started, detail))
            batch = json.loads(stdout)
            batch = batch.get('data', batch)
            if len(batch['transfers']) != FILES:
                raise RuntimeError('resumed send did not report every file')
            verify = ("from pathlib import Path\nimport hashlib\n"
                      "root=Path.home()/'Paperboat/Inbox'\n"
                      "expected=" + repr(expected) + "\n"
                      "files=sorted(root.glob(" + repr(prefix + '*') + "))\n"
                      "assert [f.name for f in files]==sorted(expected),[f.name for f in files]\n"
                      "for f in files:\n"
                      " assert f.stat().st_size==" + str(file_size) + "\n"
                      " h=hashlib.sha256()\n"
                      " with f.open('rb') as fh:\n"
                      "  for b in iter(lambda:fh.read(1<<20),b''): h.update(b)\n"
                      " assert h.hexdigest()==expected[f.name],f.name\n"
                      "print('PB_RESUME_VERIFIED')\n")
            windows = windows_target.is_windows(binary, args.target)
            output, errors = run(binary, 'exec', args.target, '--', *(
                windows_target.ps_argv(windows_target.inbox_verify_script(expected, 'PB_RESUME_VERIFIED', file_size, prefix))
                if windows else ['python3', '-c', verify]))
            if output != b'PB_RESUME_VERIFIED\n' or errors:
                raise RuntimeError('received file was not verified')
        except Exception as error:
            failure = error
            raise
        finally:
            stop_failure = None
            try:
                if process.poll() is None:
                    process.kill()
                process.close()
            except Exception as error:
                stop_failure = error
            try:
                clean_inbox()
            except Exception as error:
                if failure is None:
                    raise
                failure.add_note('Exact received-file cleanup failed: ' + str(error))
            if stop_failure is not None:
                if failure is None:
                    raise stop_failure
                failure.add_note('Stopping the send failed: ' + str(stop_failure))
    if registration() != before:
        raise RuntimeError('transfer resume changed the enrolled identity')
    print(json.dumps({'check': 'transfer-resume', 'target': args.target, 'interrupt': args.interrupt, 'status': 'PASS',
                      'bytes': FILES * file_size, 'interrupted_at_bytes': progress['bytes_sent'], 'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
