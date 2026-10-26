#!/usr/bin/env python3
"""F14-06: an official install updates itself at the configured time, and the settings persist and disable.

Usage: update_scheduled.py [--lead-minutes N]
Run on a paired, healthy official install that is older than the newest official release (`pb update
check` says an update is available). Nothing here installs: the machine must do that on its own (the release is only
downloaded ahead).
  1. disabled: `--auto=false --time T1` persists (settings and status agree, no next maintenance); the
     machine stays on its version past T1 (disabling keeps manual updates only)
  2. enabled: `--auto=true --time T2` persists with the next maintenance at T2; before T2 the machine is
     still on its version; after T2 it is on the newest official release (the `latest_version` that
     `pb update check` reported), doctor is healthy, the guard runs the same version, activation is done
     and the next maintenance moved to the next day
  3. the original settings are restored, also when a step fails.
"""
import argparse
import datetime
import json
import os
from pathlib import Path
import time

import pairing_lifecycle as p

PB = Path.home() / '.local/bin/pb'


class Clock:
    """The machine's local clock; tests replace it so no minute is really waited."""
    def now(self):
        return datetime.datetime.now().astimezone()

    def sleep(self, seconds):
        time.sleep(seconds)


clock = Clock()
READY_SECONDS = int(os.environ.get('PAPERBOAT_SMOKE_READY_SECONDS', '600'))
# A new version is not 'activation complete' until its stability window (about 10 minutes) passes; settings are refused until then.
ACTIVATION_SECONDS = int(os.environ.get('PAPERBOAT_SMOKE_ACTIVATION_SECONDS', '900'))


def run(*args, timeout=90):
    return p.invoke(PB, *args, '--json', timeout=timeout)


def settings():
    value = run('update', 'settings')
    return value.get('data', value)


def status():
    value = run('update', 'status')
    return value.get('data', value)


def version():
    return json.loads(p.invoke(PB, '--version', '--json', raw=True))['data']['version']


def at_minute(minutes_ahead):
    """The machine-local HH:MM that is MINUTES_AHEAD whole minutes after the current minute, and its time."""
    target = clock.now().replace(second=0, microsecond=0) + datetime.timedelta(minutes=minutes_ahead)
    return target, target.strftime('%H:%M')


def utc_minute(moment):
    return moment.astimezone(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M')


def wait_until(deadline, condition, what):
    while clock.now() < deadline:
        if condition():
            return
        clock.sleep(5)
    raise AssertionError('timed out waiting for ' + what)


def disabled_phase(before):
    target, text = at_minute(2)
    run('update', 'settings', '--auto=false', '--time', text)
    current = settings()
    assert current['automatic_updates'] is False and current['local_time'] == text, 'disable did not persist: %r' % current
    state = status()
    assert state['settings']['enabled'] is False, 'update status disagrees with settings: %r' % state['settings']
    clock.sleep(max(0, (target + datetime.timedelta(minutes=1) - clock.now()).total_seconds()))
    assert version() == before, 'a disabled schedule still updated the machine'
    return text


def scheduled_phase(lead, before, latest):
    target, text = at_minute(lead)
    run('update', 'settings', '--auto=true', '--time', text)
    current = settings()
    assert current['automatic_updates'] is True and current['local_time'] == text, 'enable did not persist: %r' % current
    assert current['next_maintenance_at'].startswith(utc_minute(target)), \
        'next maintenance is %r, expected %s' % (current['next_maintenance_at'], text)
    if clock.now() < target - datetime.timedelta(seconds=20):
        assert version() == before, 'the machine updated before its maintenance time'
    wait_until(target + datetime.timedelta(seconds=READY_SECONDS), lambda: version() == latest,
               'the machine to reach %s by itself' % latest)
    updated_at = clock.now()
    assert updated_at >= target - datetime.timedelta(seconds=60), \
        'updated %d s before the configured time' % (target - updated_at).total_seconds()

    def settled():
        try:  # the daemon and updater restart while the new version activates
            state = status()
            doctor = p.command_result(PB, 'doctor', '--json', timeout=60)
            report = json.loads(doctor.stdout)
        except (RuntimeError, ValueError):
            return False
        report = report.get('data', report)
        guard = [c for c in report.get('checks', []) if c.get('code') == 'machine_guard_version']
        return (state['runtime_version'] == latest and state['activation_pending'] is False
                and report.get('overall') == 'healthy' and (not guard or guard[0].get('status') == 'pass'))
    wait_until(clock.now() + datetime.timedelta(seconds=ACTIVATION_SECONDS), settled, 'doctor healthy, activation done and the guard on %s' % latest)
    next_run = status()['next_maintenance_at']
    assert next_run[:16] > utc_minute(target + datetime.timedelta(hours=1)), \
        'next maintenance did not move to the next day: %r' % next_run
    return text, round((updated_at - target).total_seconds(), 1), next_run


def restore(original):
    """Put the original schedule back; the updater may still be restarting, so retry for a while."""
    flags = ['--auto=true' if original['automatic_updates'] else '--auto=false', '--time', original['local_time']]
    deadline = clock.now() + datetime.timedelta(seconds=ACTIVATION_SECONDS)
    while True:
        try:
            run('update', 'settings', *flags)
            return
        except RuntimeError:
            if clock.now() > deadline:
                raise
            clock.sleep(5)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--lead-minutes', type=int, default=3)
    args = parser.parse_args()
    original = settings()
    before = version()
    check = run('update', 'check')
    check = check.get('data', check)
    assert check['update_available'] and check['installed_version'] == before, \
        'needs an official install older than the newest release: %r' % check
    latest = check['latest_version']
    # Only the download happens here, never the install: an installed updater without the controlMu fix blocks settings
    # reads while its automatic check downloads, which failed the schedule step below.
    run('update', 'download', timeout=300)
    try:
        disabled_time = disabled_phase(before)
        time_set, seconds_after, next_run = scheduled_phase(args.lead_minutes, before, latest)
    finally:
        restore(original)
    restored = settings()
    assert restored['local_time'] == original['local_time'] and restored['automatic_updates'] == original['automatic_updates'], \
        'original settings not restored: %r' % restored
    print(json.dumps({'check': 'update-scheduled', 'status': 'PASS', 'from': before, 'to': latest,
                      'disabled_stayed_put': True, 'configured_time': time_set, 'disabled_time': disabled_time,
                      'seconds_after_configured_time': seconds_after, 'next_maintenance_at': next_run,
                      'settings_restored': True, 'enrollment_preserved': True}), flush=True)


if __name__ == '__main__':
    main()
