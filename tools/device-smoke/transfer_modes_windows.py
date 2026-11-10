#!/usr/bin/env python3
"""F22-01 with the Windows device as the sender (Windows twin of transfer_modes.sh / transfer_modes_mac.sh).

The device (PB_SMOKE_WINDOWS_PB, SMOKE_WINDOWS_SSH: winhost.py) sends to ALIAS, a paired Unix machine of the same
account; TARGET_CMD ends before ONE shell-string argument (quoted here for ssh's remote shell) and runs it on that machine as its pb user (for example
`ssh host sudo -n docker exec -u pbt -e HOME=/home/pbt CONTAINER bash -c`). Same steps and pass conditions as the Unix
twins: automatic 120 MiB + a 600 MiB batch arrive byte-identical, 2.2 GB is refused before data moves; unlimited 300 MiB
with the device's UDP blocked (a firewall rule, winhost.udp_blocked) waits, then resumes byte-identical; losing the path
mid-send stops it at once (the device's egress over 30 s < 3 MB), survives `pb service restart`, resumes.
Usage: TARGET_CMD=... transfer_modes_windows.py ALIAS     Prints one JSON line; exit 1 on FAIL.
"""
import contextlib
import json
import os
import shlex
import subprocess
import sys
import time
import uuid

import winhost

DEVICE = winhost.device()
PB = winhost.pb_path()
MIB = 1 << 20
TX = ("(Get-NetAdapterStatistics|Where-Object{$_.Name -notmatch 'Loopback'}|Measure-Object -Property SentBytes -Sum).Sum")
R = []
TRACE = []


def target_argv(script):
    # ssh joins its arguments and the remote shell splits them again: an unquoted script lost everything after its first word
    # (`sha256sum f | cut` ran as `bash -c sha256sum` piped on the host, so every arrival check hashed empty input).
    return shlex.split(os.environ['TARGET_CMD']) + [shlex.quote(script)]


def target(script, timeout=300):
    done = subprocess.run(target_argv(script), capture_output=True, text=True, timeout=timeout, stdin=subprocess.DEVNULL)
    return done.stdout.strip()


def arrived(name, sha):
    got = target('sha256sum ~/Paperboat/Inbox/%s 2>/dev/null | cut -d" " -f1' % shlex.quote(name))
    TRACE.append('%s arrived=%s want=%s' % (name, got[:12] or 'none', sha[:12]))
    return got == sha


def tx():
    return int(DEVICE.ps('egress bytes', TX)[1])


def send(alias, paths, *flags, timeout=900):
    # A FAIL names the exit code, seconds and stderr of every send: "failed" alone cannot tell a stall from a refusal.
    start, name = time.monotonic(), os.path.basename(str(paths[0]).replace('\\', '/'))
    try:
        done = DEVICE.run([DEVICE.pb_exe, 'send', *map(str, paths), '--to', alias, *flags], timeout=timeout)
    except subprocess.TimeoutExpired:
        TRACE.append('send %s timeout after %ds' % (name, time.monotonic() - start))
        raise
    TRACE.append('send %s rc=%s %ds err=%s' % (name, done.returncode, time.monotonic() - start, (done.stderr or '')[-160:].strip()))
    return done


def main():
    alias = sys.argv[1]
    work = DEVICE.temp_dir('pb-xfer')
    scratch = work.__enter__()
    stack, detail, job = contextlib.ExitStack(), '', None
    try:
        target('mkdir -p ~/Paperboat/Inbox; ~/.local/bin/pb inbox policy automatic >/dev/null; cd ~/Paperboat/Inbox && rm -f x-auto.bin b?.bin x-unl.bin')
        auto = scratch / 'x-auto.bin'
        sha = auto.write_random(120 * MIB)
        R.append('AUTOMATIC_OK' if send(alias, [auto]).returncode == 0 and arrived('x-auto.bin', sha) else 'AUTOMATIC_FAILED')

        batch, want = [], []
        for i in range(1, 7):
            batch.append(scratch / ('b%d.bin' % i))
            want.append(batch[-1].write_random(100 * MIB))
        R.append('BATCH_OK' if send(alias, batch).returncode == 0 and
                 target('cd ~/Paperboat/Inbox && sha256sum b?.bin | cut -d" " -f1 | sort').split() == sorted(want) else 'BATCH_FAILED')

        huge = scratch / 'x-huge.bin'
        huge.truncate(2200 * MIB)
        done = send(alias, [huge], timeout=300)
        refused = 'Automatic sends up to' in done.stdout + done.stderr and not target('test -e ~/Paperboat/Inbox/x-huge.bin && echo yes')
        R.append('HUGE_ACCEPTED' if done.returncode == 0 else 'HUGE_REFUSED' if refused else 'HUGE_FAILED')
        huge.unlink()

        unl = scratch / 'x-unl.bin'
        sha = unl.write_random(300 * MIB)
        stack.enter_context(DEVICE.udp_blocked())
        time.sleep(20)  # the direct path ages out
        job = winhost.job(PB, ['send', str(unl), '--to', alias, '--unlimited'])
        waited = False
        for _ in range(120):
            text = (job.stdout() + job.stderr()).decode(errors='replace').lower()
            if job.poll() is not None:
                break
            if any(word in text for word in ('wait', 'direct', 'relay')):
                waited = True
                break
            time.sleep(1)
        time.sleep(10)
        R.append('UNLIMITED_PAUSED' if waited and not arrived('x-unl.bin', sha) else 'UNLIMITED_NOT_PAUSED')
        stack.close()  # UDP unblocked
        R.append('UNLIMITED_RESUMED' if job.wait(900) == 0 and arrived('x-unl.bin', sha) else 'UNLIMITED_FAILED')
        job.close()

        target('rm -f ~/Paperboat/Inbox/x-unl.bin')
        job = winhost.job(PB, ['send', str(unl), '--to', alias, '--unlimited'])
        start = tx()
        for _ in range(120):
            if tx() - start >= 60_000_000:
                break
            time.sleep(1)
        stack.enter_context(DEVICE.udp_blocked())
        time.sleep(10)  # bytes in flight drain
        before = tx()
        time.sleep(30)
        leaked = tx() - before
        R.append('LOSS_STOPPED' if leaked < 3_000_000 and job.poll() is None and not arrived('x-unl.bin', sha)
                 else 'LOSS_LEAKED_%d' % leaked)
        DEVICE.run([DEVICE.pb_exe, 'service', 'restart'], timeout=180)  # the paused send must survive its daemon restarting
        stack.close()
        R.append('LOSS_RESUMED' if job.wait(900) == 0 and arrived('x-unl.bin', sha) else 'LOSS_FAILED')
        wanted = ('AUTOMATIC_OK', 'BATCH_OK', 'HUGE_REFUSED', 'UNLIMITED_PAUSED', 'UNLIMITED_RESUMED', 'LOSS_STOPPED', 'LOSS_RESUMED')
        missing = [step for step in wanted if step not in R]
        detail = ('missing ' + ' '.join(missing)) if missing else (
            'automatic 120 MiB + 600 MiB batch byte-identical, 2.2 GB refused; unlimited waited under the UDP block and resumed; '
            'path loss leaked %d B in 30 s, survived a restart, resumed' % leaked)
        status = 'FAIL' if missing else 'PASS'
    except BaseException as error:
        status, detail = 'FAIL', '%s: %s' % (type(error).__name__, str(error)[:300])
    finally:
        stack.close()
        if job is not None:
            with contextlib.suppress(Exception):
                job.kill()
                job.close()
        with contextlib.suppress(Exception):
            target('cd ~/Paperboat/Inbox && rm -f x-auto.bin b?.bin x-unl.bin x-huge.bin')
        with contextlib.suppress(Exception):
            work.__exit__(None, None, None)
    print(json.dumps({'check': 'transfer-modes-windows', 'status': status, 'steps': R, 'detail': detail, 'trace': TRACE}), flush=True)
    sys.exit(0 if status == 'PASS' else 1)


if __name__ == '__main__':
    if not DEVICE:
        sys.exit('needs PB_SMOKE_WINDOWS_PB (the Windows device is the sender)')
    main()
