#!/usr/bin/env python3
"""Interrupt a large batch mid-send; every file must arrive once, byte-identical, and the same send finishes by itself.

--interrupt receiver (default): restart the receiver's daemon. sender: restart this machine's daemon.
network: drop this user's outbound non-loopback traffic for DROP_SECONDS (iptables, needs sudo -n; removed on
every exit path and by a root failsafe timer, as network_drop.py does).

Proof that the interruption landed mid-batch: it is asked for only after a progress event showed bytes outstanding, and
the receiver's own publish markers must show the last file published after the restarted daemon started (or after the
drop ended). The send may finish as fast as it likes after that.
"""
import argparse
import json
import os
import re
import subprocess
import sys
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
# The interruption starts at the first progress event (`pb send --json` writes one to stderr every 5 s).
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


def in_flight(process, ended, now=time.monotonic, sleep=time.sleep):
    """Wait for a progress event showing 0 < bytes_sent < total; fail with `ended` if the send ends first. That event
    is the proof the batch was in flight at the moment it was written."""
    deadline = now() + PROGRESS_SECONDS
    while True:
        events = progress_events(process.stderr())
        newest = events[-1] if events else None
        if newest and 0 < newest['bytes_sent'] < newest['total_bytes']:
            return newest
        if process.poll() is not None:
            raise RuntimeError(ended)
        if now() > deadline:
            raise RuntimeError('the send showed no progress within %ds' % PROGRESS_SECONDS)
        sleep(1)


# ---- the proof: clocks, the restarted daemon and the publish times -------------------------------------
# Each pair is (a python3 script for a Unix host, a PowerShell script for a Windows one). Times are epoch seconds on
# the host that runs the script; PowerShell prints whole milliseconds (no locale decimal separator).
CLOCK = ('import time; print(time.time())', '[Console]::Out.Write([DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds())')

# Prints "<start of the running daemon> <how much later than that it may really have started>".
DAEMON_STARTED = ("""import os, subprocess, sys, time
if sys.platform == 'linux':
    env = dict(os.environ)
    env.setdefault('XDG_RUNTIME_DIR', '/run/user/%d' % os.getuid())
    pid = int(subprocess.check_output(['systemctl', '--user', 'show', '-p', 'MainPID', '--value', 'paperboatd.service'], env=env))
    ticks = int(open('/proc/%d/stat' % pid).read().rsplit(')', 1)[1].split()[19])  # field 22: start, in ticks since boot
    print(time.time() - time.clock_gettime(time.CLOCK_BOOTTIME) + ticks / os.sysconf('SC_CLK_TCK'), 0.05)
else:  # macOS: ps gives the start to the whole second
    rows = subprocess.check_output(['ps', '-u', str(os.getuid()), '-o', 'lstart=,command='], env=dict(os.environ, LC_ALL='C'), text=True)
    [row] = [row for row in rows.splitlines() if '/pb daemon --config ' in row]
    print(time.mktime(time.strptime(' '.join(row.split()[:5]), '%a %b %d %H:%M:%S %Y')), 1.0)
""", """$s=Get-CimInstance Win32_Service -Filter "Name='PaperboatLocalDaemon'"
if(-not $s.ProcessId){throw 'the daemon service is not running'}
$p=Get-CimInstance Win32_Process -Filter ('ProcessId='+$s.ProcessId)
[Console]::Out.Write("$(([DateTimeOffset]$p.CreationDate).ToUnixTimeMilliseconds()) 1")
""")

def on_target(binary, target, windows, scripts):
    argv = windows_target.ps_argv(scripts[1]) if windows else ['python3', '-c', scripts[0]]
    return run(binary, 'exec', target, '--', *argv)[0].decode()


def on_source(scripts):
    """The sending machine: this one, or the Windows device."""
    device = winhost.device()
    done = (device.run(windows_target.ps_argv(scripts[1]), timeout=60) if device else
            subprocess.run([sys.executable, '-c', scripts[0]], capture_output=True, text=True, timeout=60))
    if done.returncode:
        raise RuntimeError('a proof script failed on the sending machine (exit %d)' % done.returncode)
    return done.stdout


def clock_seconds(text, windows):
    return float(text.strip()) / (1000 if windows else 1)


def daemon_started(text, windows):
    """(start, latest possible start) of the running daemon, from DAEMON_STARTED's output."""
    started, slack = (clock_seconds(word, windows) for word in text.split())
    return started, started + slack


def published_scripts(transfer_ids):
    """Prints each transfer's publish time. The receiver's runtime writes the empty <id>.published marker beside its
    spool right before it puts that file in the Inbox (cli/internal/hostruntime/filetransfer/publication.go), so the
    marker's mtime is on the receiver's clock and never later than the publish; a missing marker fails the script."""
    if not transfer_ids or not all(re.fullmatch(r'transfer_[0-9a-f-]+', i) for i in transfer_ids):
        raise RuntimeError('the send reported no usable transfer ids')
    unix = ("from pathlib import Path\nimport sys\n"
            "root = Path.home() / ('.local/state/paperboat' if sys.platform == 'linux' else 'Library/Application Support/Paperboat')\n"
            "for i in %r: print((root / 'runtime/file-transfers' / (i + '.published')).stat().st_mtime)" % list(transfer_ids))
    windows = ("$s=if($env:XDG_STATE_HOME){$env:XDG_STATE_HOME}else{$env:LOCALAPPDATA}\n"
               "foreach($i in @(%s)){$f=Get-Item -LiteralPath (Join-Path $s ('Paperboat\\runtime\\file-transfers\\'+$i+'.published')) "
               "-Force -ErrorAction Stop\n[Console]::Out.WriteLine(([DateTimeOffset]$f.LastWriteTimeUtc).ToUnixTimeMilliseconds())}"
               % ','.join("'%s'" % i for i in transfer_ids))
    return unix, windows


def last_published(text, count, windows):
    """When the receiver published the last of the `count` transfers, from published_scripts' output."""
    times = [clock_seconds(word, windows) for word in text.split()]
    if len(times) != count or not times:
        raise RuntimeError('the receiver reported %d publish times for %d transfers' % (len(times), count))
    return max(times)


def clock_ahead(receiver_clock, source_clock=None, samples=3, now=None):
    """Upper bound, in seconds, of the receiver's clock minus the sending machine's. Each argument reads that host's
    clock over a round trip timed here; source_clock None means the sending machine is this one."""
    now = now or time.time

    def bounds(read):  # (lower, upper) of that host's clock minus this one's
        before = now()
        value = read()
        return value - now(), value - before
    receiver = min(bounds(receiver_clock)[1] for _ in range(samples))
    return receiver - (max(bounds(source_clock)[0] for _ in range(samples)) if source_clock else 0)


def prove_landed(what, issued, began, ended, published, ahead=0.0):
    """Seconds between the interruption having taken hold and the last publish; raises unless it landed mid-batch.

    issued (read after the in-flight progress event, before the interruption was asked for), began (when it took hold
    at the earliest) and ended (the latest moment a file could have been published without it mattering: a restart's
    latest possible start, a drop's removal) are on the interrupted host's clock; published is on the receiver's,
    which is at most `ahead` seconds ahead of that one."""
    if began < issued:
        raise RuntimeError('the %s never happened: the daemon has been running since %.1f s before it was asked for'
                           % (what, issued - began))
    margin = published - (ended + ahead)
    if margin <= 0:
        raise RuntimeError('the %s did not land while the batch was in flight: the last file was published %.2f s '
                           'before it had taken hold' % (what, -margin))
    return round(margin, 2)


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

        windows = windows_target.is_windows(binary, args.target)
        source_windows = winhost.device() is not None

        def interrupt():
            """(issued, drop inserted, drop removed) on the interrupted host's clock; a restart has no drop."""
            command = '(sleep 1; ~/.local/bin/pb service restart) >/dev/null 2>&1 &'
            if args.interrupt == 'receiver':  # the same command reads the receiver's clock before it restarts
                argv = (windows_target.ps_argv(CLOCK[1] + '\n' + windows_target.restart_service_detached_script(
                            windows_target.target_pb(binary, args.target))) if windows
                        else ['sh', '-c', "python3 -c '%s' && { %s }" % (CLOCK[0], command)])
                return clock_seconds(run(binary, 'exec', args.target, '--', *argv)[0].decode(), windows), None, None
            issued = clock_seconds(on_source(CLOCK), source_windows)
            if args.interrupt == 'sender' and source_windows:
                binary.run_remote(['service', 'restart'], timeout=120)
            elif args.interrupt == 'sender':
                subprocess.run(['sh', '-c', command], timeout=30, check=True)
            else:
                # Root failsafe removes the rule even if this process dies.
                subprocess.Popen(['sudo', '-n', 'sh', '-c', 'sleep %d; iptables -D %s' % (DROP_SECONDS + 60, ' '.join(RULE))],
                                 stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
                subprocess.run(['sudo', '-n', 'iptables', '-I', *RULE], timeout=30, check=True)
                inserted = time.time()
                try:
                    time.sleep(DROP_SECONDS)
                finally:
                    removed = time.time()
                    subprocess.run(['sudo', '-n', 'iptables', '-D', *RULE], timeout=30)
                return issued, inserted, removed
            return issued, None, None

        def prove(issued, inserted, removed, published):
            if args.interrupt == 'network':
                ahead = clock_ahead(lambda: clock_seconds(on_target(binary, args.target, windows, CLOCK), windows))
                return prove_landed('network drop', issued, inserted, removed, published, ahead)
            if args.interrupt == 'receiver':
                text = on_target(binary, args.target, windows, DAEMON_STARTED)
                return prove_landed('receiver daemon restart', issued, *daemon_started(text, windows), published)
            began, latest = daemon_started(on_source(DAEMON_STARTED), source_windows)
            ahead = clock_ahead(lambda: clock_seconds(on_target(binary, args.target, windows, CLOCK), windows),
                                (lambda: clock_seconds(on_source(CLOCK), True)) if source_windows else None)
            return prove_landed('sender daemon restart', issued, began, latest, published, ahead)

        process = winhost.job(binary, ['send', *sources, '--to', args.target, '--json'])
        try:
            started = time.monotonic()
            progress = in_flight(process, 'the send ended before a progress event showed it in flight; the %d MiB fixture '
                                 'is too small for this path' % (FILES * file_size >> 20))
            interruption = interrupt()
            time.sleep(SETTLE_SECONDS)
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
            output, errors = run(binary, 'exec', args.target, '--', *(
                windows_target.ps_argv(windows_target.inbox_verify_script(expected, 'PB_RESUME_VERIFIED', file_size, prefix))
                if windows else ['python3', '-c', verify]))
            if output != b'PB_RESUME_VERIFIED\n' or errors:
                raise RuntimeError('received file was not verified')
            ids = [t['transfer_id'] for t in batch['transfers']]
            published = last_published(on_target(binary, args.target, windows, published_scripts(ids)), len(ids), windows)
            margin = prove(*interruption, published)
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
                      'bytes': FILES * file_size, 'interrupted_at_bytes': progress['bytes_sent'],
                      'published_after_interruption_s': margin, 'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
