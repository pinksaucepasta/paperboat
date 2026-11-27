#!/usr/bin/env python3
"""F14-06b: a signed release is installed only at the update window, never right after an install or a manual update,
and a machine that missed its window catches up when it wakes (smoke 11-26: mp5 and lp12 installed the newest signed
release ~30 s after a manual update, outside the 04:00 window).

Usage: update_window_hold.py --scenario hold|missed --installed A --latest B [--wait-latest SECONDS]
Run on a paired, healthy official install of A with automatic updates on, while B (newer, official) is published or
about to be: the row first waits up to --wait-latest for `pb update check` to report B.
  hold   (AU1) the window is set 14 min ahead. Through an updater restart and a failed `pb update --from` (the 11-26
         trigger), the OFFICIAL install stays on A until 1 min before the window. Then a successful same-version
         `pb update --from` makes it custom, and it stays on A past the window too (a supplied build never updates itself).
  missed (AU3) the window is set 2 min ahead and the updater is frozen across it, as on a machine asleep at its window,
         for at most ~150 s: under the unit's 300 s watchdog, so the same process wakes (its pid is checked; a watchdog
         restart would only prove the startup catch-up). Windows has no freeze: the service is stopped and started,
         which proves the catch-up of a cold start (reported as such). B is installed and its activation completes.
The original settings are restored when the machine is still official (hold leaves a custom install behind); after a
failure that left it on A, automatic updates stay off (the next run turns them on with its own window).
"""
import argparse
import datetime
import json
import os
import shutil
import subprocess
import tempfile

import pairing_lifecycle as p
import update_health
import update_scheduled as u
import winhost

PB = winhost.pb_path()
HOLD_LEAD, MISSED_LEAD, WAKE_AFTER_SECONDS = 14, 2, 30
POLL = 60
MISSING_WORDS = ('no such file', 'not found', 'does not exist', 'cannot find', 'missing', 'could not read', 'cannot be read', 'not a file')
relabelled = []  # set once a same-version --from made the install custom: its settings are not restored


def retried(read, attempts=4):
    """A read that can fail while an activation swaps the binary or restarts the daemon: tried again for ~30 s."""
    for attempt in range(attempts):
        try:
            return read()
        except (RuntimeError, ValueError, p.CommandTimeout):
            if attempt == attempts - 1:
                raise
            u.clock.sleep(10)


def check():
    value = retried(lambda: u.run('update', 'check', timeout=120))
    return value.get('data', value)


def version():
    return retried(u.version)


def updater_pid():
    """The main pid of this account's updater (systemd unit or launchd job), never another account's."""
    uid = os.getuid()
    if shutil.which('systemctl'):
        out = subprocess.run(['systemctl', 'show', '-p', 'MainPID', '--value', 'paperboat-updated-u%d.service' % uid],
                             capture_output=True, text=True, timeout=30).stdout.strip()
    else:
        out = subprocess.run(['sudo', '-n', 'launchctl', 'print', 'system/com.pinksaucepasta.paperboat.updated.u%d' % uid],
                             capture_output=True, text=True, timeout=30).stdout
        out = next((line.split('=', 1)[1].strip() for line in out.splitlines() if line.strip().startswith('pid =')), '')
    assert out.isdigit() and out != '0', 'no running updater for uid %d' % uid
    return out


def updater(action, pid=None):
    """Restart, freeze or wake the signed update service of this machine (sudo on Unix; Windows: the service).
    freeze returns the frozen pid; wake takes it."""
    device = winhost.device()
    if device:
        script = {'restart': "Restart-Service -Name 'PaperboatUpdated-*' -Force",
                  'freeze': "Stop-Service -Name 'PaperboatUpdated-*' -Force",
                  # a service something else started again while "asleep" would hide the missed window: say so
                  'wake': "if((Get-Service -Name 'PaperboatUpdated-*').Status -ne 'Stopped'){throw 'the updater was started while it should sleep'};"
                          "Start-Service -Name 'PaperboatUpdated-*'"}[action]
        device.ps('updater ' + action, script, timeout=120)
        return None
    uid = os.getuid()
    if action == 'restart':
        if shutil.which('systemctl'):
            command = ['sudo', '-n', 'systemctl', 'restart', 'paperboat-updated-u%d.service' % uid]
        else:
            command = ['sudo', '-n', 'launchctl', 'kickstart', '-k', 'system/com.pinksaucepasta.paperboat.updated.u%d' % uid]
        subprocess.run(command, check=True, capture_output=True, text=True, timeout=120)
        return
    if action == 'freeze':
        pid = updater_pid()
        subprocess.run(['sudo', '-n', 'kill', '-STOP', pid], check=True, capture_output=True, text=True, timeout=30)
        return pid
    subprocess.run(['sudo', '-n', 'kill', '-CONT', pid], capture_output=True, text=True, timeout=30)  # best effort: always tried
    return pid


def stays_on(installed, until, samples, what):
    """Polls every POLL s until UNTIL; every sample must still run INSTALLED."""
    while u.clock.now() < until:
        seen = version()
        samples.append({'at': u.clock.now().isoformat(timespec='seconds'), 'version': seen, 'phase': what})
        assert seen == installed, '%s: installed %s outside the window (expected %s)' % (what, seen, installed)
        u.clock.sleep(min(POLL, max(1, (until - u.clock.now()).total_seconds())))


def hold(installed, samples):
    window, text = u.at_minute(HOLD_LEAD)
    u.run('update', 'settings', '--auto=true', '--time', text)
    start = u.clock.now()
    updater('restart')
    stays_on(installed, start + datetime.timedelta(minutes=1), samples, 'after an updater restart')
    device = winhost.device()
    missing = (device.ps('missing path', "Join-Path $env:TEMP ('pb-missing-'+[guid]::NewGuid()+'.exe')")[1].strip() if device
               else os.path.join(tempfile.gettempdir(), 'pb-missing-%d' % os.getpid()))
    failed = p.command_result(PB, 'update', '--from', missing, timeout=300)
    said = (failed.stdout + failed.stderr).lower()
    assert failed.returncode != 0, 'pb update --from a missing file succeeded'
    assert any(word in said for word in MISSING_WORDS), 'pb update --from failed for another reason than the missing file: %s' % said[-300:]
    # the official install, the window not yet reached: the 11-26 machines installed B here
    stays_on(installed, window - datetime.timedelta(minutes=1), samples, 'official, after a failed pb update --from')
    if device:
        copy = device.ps('copy pb', "$d=Join-Path $env:TEMP ('pb-same-'+[guid]::NewGuid());"
                         "New-Item -ItemType Directory $d|Out-Null;Copy-Item -LiteralPath %s (Join-Path $d 'pb.exe');"
                         "Join-Path $d 'pb.exe'" % winhost.ps_quote(str(PB)))[1].strip()
        try:
            same = p.command_result(PB, 'update', '--from', copy, timeout=900)
        finally:
            device.ps('remove copy', 'Remove-Item -Recurse -Force -LiteralPath %s' % winhost.ps_quote(copy.rsplit('\\', 1)[0]), ok=(0, 1))
    else:
        with tempfile.TemporaryDirectory() as folder:
            copy = os.path.join(folder, 'pb-same')
            shutil.copy2(os.path.realpath(PB), copy)
            same = p.command_result(PB, 'update', '--from', copy, timeout=900)
    assert same.returncode == 0, 'same-version pb update --from failed: %s' % (same.stdout + same.stderr)[-400:]
    relabelled.append(True)
    health = update_health.settled(installed, u.READY_SECONDS, pb=PB, status=u.status, version=version, clock=u.clock_seconds)
    samples.append({'at': u.clock.now().isoformat(timespec='seconds'), 'phase': 'healthy after the same-version --from', **health})
    stays_on(installed, window + datetime.timedelta(minutes=3), samples, 'custom after a same-version pb update --from')
    return text


def missed(installed, latest, samples):
    window, text = u.at_minute(MISSED_LEAD)
    u.run('update', 'settings', '--auto=true', '--time', text)
    frozen = None
    try:
        frozen = updater('freeze')
        stays_on(installed, window + datetime.timedelta(seconds=WAKE_AFTER_SECONDS), samples, 'asleep across the window')
    finally:
        if frozen or winhost.device():
            updater('wake', frozen)
    woke = u.clock.now()
    if frozen:  # the same process woke: a watchdog restart in between would prove only the startup catch-up
        assert updater_pid() == frozen, 'the updater was restarted while frozen (watchdog?): not a wake from sleep'
    else:
        samples.append({'at': woke.isoformat(timespec='seconds'), 'phase': 'Windows: cold start of the stopped service'})
    u.wait_until(woke + datetime.timedelta(seconds=u.READY_SECONDS + u.ACTIVATION_SECONDS),
                 lambda: version() == latest and not u.activation_pending(), 'the missed window to install ' + latest)
    samples.append({'at': u.clock.now().isoformat(timespec='seconds'), 'version': latest, 'phase': 'caught up'})
    health = update_health.settled(latest, u.ACTIVATION_SECONDS, pb=PB, status=u.status, version=version, clock=u.clock_seconds)
    samples.append({'at': u.clock.now().isoformat(timespec='seconds'), 'phase': 'healthy on ' + latest, **health})
    return text, int((u.clock.now() - woke).total_seconds())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--scenario', choices=('hold', 'missed', 'wake'), required=True, help='wake: only SIGCONT this account\'s updater (cleanup)')
    parser.add_argument('--installed', required=True, help='release A this machine runs')
    parser.add_argument('--latest', required=True, help='release B, newer, that the machine must not install early')
    parser.add_argument('--wait-latest', type=int, default=3600, help='seconds to wait for B to be published')
    args = parser.parse_args()
    if args.scenario == 'wake':  # smoke.sh's cleanup after a missed row, also when the row was killed while it slept
        if not winhost.device():
            updater('wake', updater_pid())
        return
    running = version()
    assert running == args.installed, 'this machine runs %s, not release A %s' % (running, args.installed)
    original = u.settings()
    published = u.clock.now() + datetime.timedelta(seconds=args.wait_latest)
    u.wait_until(published, lambda: check().get('latest_version') == args.latest, 'release %s to be published' % args.latest)
    samples, result, passed = [], {'check': 'update-window-' + args.scenario, 'installed': args.installed, 'latest': args.latest}, False
    try:
        if args.scenario == 'hold':
            result['window'] = hold(args.installed, samples)
            result['stayed_on'] = args.installed
        else:
            result['window'], result['seconds_to_catch_up'] = missed(args.installed, args.latest, samples)
        passed = True
    finally:
        result['samples'] = samples
        if not relabelled:
            # A window already passed today is due as soon as it is set: after a failure on A, restoring it would
            # install B and destroy the rerun's premise (smoke dry run 11-27), so updates stay off there.
            try:
                stayed = not passed and version() == args.installed
            except (RuntimeError, ValueError, p.CommandTimeout):
                stayed = not passed  # unreadable: off is the side that installs nothing
            if stayed:
                original = dict(original, automatic_updates=False)
            u.restore(original)
    result.update(status='PASS', enrollment_preserved=True)
    print(json.dumps(result), flush=True)


if __name__ == '__main__':
    main()
