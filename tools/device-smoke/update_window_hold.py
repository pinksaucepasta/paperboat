#!/usr/bin/env python3
"""F14-06b: a signed release is installed only at the update window, never right after an install or a manual update,
and a machine that missed its window catches up when it wakes (smoke 11-26: mp5 and lp12 installed the newest signed
release ~30 s after a manual update, outside the 04:00 window).

Usage: update_window_hold.py --scenario hold|missed --installed A --latest B [--wait-latest SECONDS]
Run on a paired, healthy official install of A with automatic updates on, while B (newer, official) is published or
about to be: the row first waits up to --wait-latest for `pb update check` to report B.
  hold   (AU1) the window is set 14 min ahead. Through an updater restart, a failed `pb update --from` and, 5 min in,
         a successful same-version `pb update --from`, the machine stays on A; it still is 3 min after the window
         (the successful --from makes the install custom, so nothing installs on its own any more).
  missed (AU3) the window is set 3 min ahead and the updater is frozen (SIGSTOP; Windows: service stopped) across
         it, as on a machine asleep at its window. 2 min after the window it wakes; B is installed and its activation
         completes within the activation bound.
The original settings are restored when the machine is still official (hold leaves a custom install behind).
"""
import argparse
import datetime
import json
import os
import shutil
import subprocess
import tempfile

import pairing_lifecycle as p
import update_scheduled as u
import winhost

PB = winhost.pb_path()
HOLD_LEAD, MISSED_LEAD, WAKE_AFTER = 14, 3, 2
POLL = 60
relabelled = []  # set once a same-version --from made the install custom: its settings are not restored


def check():
    value = u.run('update', 'check', timeout=120)
    return value.get('data', value)


def updater(action):
    """Restart, freeze or wake the signed update service of this machine (sudo on Unix; Windows: the service)."""
    device = winhost.device()
    if device:
        script = {'restart': "Restart-Service -Name 'PaperboatUpdated-*' -Force",
                  'freeze': "Stop-Service -Name 'PaperboatUpdated-*' -Force",
                  'wake': "Start-Service -Name 'PaperboatUpdated-*'"}[action]
        device.ps('updater ' + action, script, timeout=120)
        return
    uid = os.getuid()
    if action == 'restart':
        if shutil.which('systemctl'):
            command = ['sudo', '-n', 'systemctl', 'restart', 'paperboat-updated-u%d.service' % uid]
        else:
            command = ['sudo', '-n', 'launchctl', 'kickstart', '-k', 'system/com.pinksaucepasta.paperboat.updated.u%d' % uid]
        subprocess.run(command, check=True, capture_output=True, text=True, timeout=120)
        return
    found = subprocess.run(['pgrep', '-f', '__runtime-updated'], capture_output=True, text=True, timeout=30).stdout.split()
    assert found, 'no updater process to %s' % action
    subprocess.run(['sudo', '-n', 'kill', '-STOP' if action == 'freeze' else '-CONT', *found],
                   check=True, capture_output=True, text=True, timeout=30)


def stays_on(installed, until, samples, what):
    """Polls every POLL s until UNTIL; every sample must still run INSTALLED."""
    while u.clock.now() < until:
        seen = u.version()
        samples.append({'at': u.clock.now().isoformat(timespec='seconds'), 'version': seen, 'phase': what})
        assert seen == installed, '%s: installed %s outside the window (expected %s)' % (what, seen, installed)
        u.clock.sleep(min(POLL, max(1, (until - u.clock.now()).total_seconds())))


def hold(installed, samples):
    window, text = u.at_minute(HOLD_LEAD)
    u.run('update', 'settings', '--auto=true', '--time', text)
    start = u.clock.now()
    updater('restart')
    stays_on(installed, start + datetime.timedelta(minutes=1), samples, 'after an updater restart')
    failed = p.command_result(PB, 'update', '--from', os.path.join(tempfile.gettempdir(), 'pb-missing-%d' % os.getpid()),
                              timeout=300)
    assert failed.returncode != 0, 'pb update --from a missing file succeeded'
    stays_on(installed, start + datetime.timedelta(minutes=5), samples, 'after a failed pb update --from')
    with tempfile.TemporaryDirectory() as folder:
        copy = os.path.join(folder, 'pb-same' + ('.exe' if winhost.device() else ''))
        if winhost.device():
            copy = winhost.device().ps('copy pb', "$d=Join-Path $env:TEMP ('pb-same-'+[guid]::NewGuid());"
                                       "New-Item -ItemType Directory $d|Out-Null;Copy-Item -LiteralPath %s (Join-Path $d 'pb.exe');"
                                       "Join-Path $d 'pb.exe'" % winhost.ps_quote(str(PB)))[1].strip()
        else:
            shutil.copy2(os.path.realpath(PB), copy)
        same = p.command_result(PB, 'update', '--from', copy, timeout=900)
        assert same.returncode == 0, 'same-version pb update --from failed: %s' % (same.stdout + same.stderr)[-400:]
        relabelled.append(True)
    stays_on(installed, window + datetime.timedelta(minutes=3), samples, 'after a same-version pb update --from')
    return text


def missed(installed, latest, samples):
    window, text = u.at_minute(MISSED_LEAD)
    u.run('update', 'settings', '--auto=true', '--time', text)
    updater('freeze')
    try:
        stays_on(installed, window + datetime.timedelta(minutes=WAKE_AFTER), samples, 'asleep across the window')
    finally:
        updater('wake')
    woke = u.clock.now()
    u.wait_until(woke + datetime.timedelta(seconds=u.READY_SECONDS + u.ACTIVATION_SECONDS),
                 lambda: u.version() == latest and not u.activation_pending(), 'the missed window to install ' + latest)
    samples.append({'at': u.clock.now().isoformat(timespec='seconds'), 'version': latest, 'phase': 'caught up'})
    return text, int((u.clock.now() - woke).total_seconds())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--scenario', choices=('hold', 'missed'), required=True)
    parser.add_argument('--installed', required=True, help='release A this machine runs')
    parser.add_argument('--latest', required=True, help='release B, newer, that the machine must not install early')
    parser.add_argument('--wait-latest', type=int, default=3600, help='seconds to wait for B to be published')
    args = parser.parse_args()
    assert u.version() == args.installed, 'this machine runs %s, not release A %s' % (u.version(), args.installed)
    original = u.settings()
    published = u.clock.now() + datetime.timedelta(seconds=args.wait_latest)
    u.wait_until(published, lambda: check().get('latest_version') == args.latest, 'release %s to be published' % args.latest)
    samples, result = [], {'check': 'update-window-' + args.scenario, 'installed': args.installed, 'latest': args.latest}
    try:
        if args.scenario == 'hold':
            result['window'] = hold(args.installed, samples)
            result['stayed_on'] = args.installed
        else:
            result['window'], result['seconds_to_catch_up'] = missed(args.installed, args.latest, samples)
    finally:
        result['samples'] = samples
        if not relabelled:
            u.restore(original)
    result.update(status='PASS', enrollment_preserved=True)
    print(json.dumps(result), flush=True)


if __name__ == '__main__':
    main()
