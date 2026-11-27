import datetime
import json
import unittest
from unittest import mock

import update_scheduled
import update_window_hold as h


class Machine:
    """An official machine on A with B published; it installs B when its window fires while the updater is awake,
    or at once when EAGER (the 11-26 bug: a version chosen outside the window replaced at the next check)."""

    def __init__(self, eager=False):
        self.time = datetime.datetime(2026, 11, 27, 9, 0, 30, tzinfo=datetime.timezone.utc)
        self.version, self.latest, self.local_time, self.enabled = '2026.11.27.0', '2026.11.27.3', '04:00', True
        self.eager, self.frozen, self.missed, self.actions, self.froms = eager, False, False, [], []

    def now(self):
        return self.time

    def sleep(self, seconds):
        before = self.time
        self.time += datetime.timedelta(seconds=seconds)
        hour, minute = map(int, self.local_time.split(':'))
        fired = self.time.replace(hour=hour, minute=minute, second=0, microsecond=0)
        if before < fired <= self.time:
            if self.frozen:
                self.missed = True
            elif self.enabled:
                self.version = self.latest

    def invoke(self, binary, *args, raw=False, **kwargs):
        if args[:1] == ('--version',):
            return json.dumps({'ok': True, 'data': {'version': self.version}})
        if args[1] == 'settings':
            for index, value in enumerate(args):
                if value.startswith('--auto='):
                    self.enabled = value.endswith('true')
                if value == '--time':
                    self.local_time = args[index + 1]
            return {'data': {'automatic_updates': self.enabled, 'local_time': self.local_time}}
        if args[1] == 'status':
            return {'data': {'activation_pending': False}}
        if args[1] == 'check':
            return {'data': {'installed_version': self.version, 'latest_version': self.latest}}
        raise AssertionError(args)

    def updater(self, action, pid=None):
        self.actions.append(action)
        if action == 'restart' and self.eager:
            self.version = self.latest
        self.frozen = action == 'freeze'
        if action == 'wake' and self.missed and self.enabled:
            self.version = self.latest
        return '42' if action in ('freeze', 'wake') else None

    def command(self, binary, *args, timeout=0, **kwargs):
        self.froms.append(args[-1])
        ok = 'pb-missing' not in args[-1]
        if ok:
            self.enabled = False  # a supplied build is custom: no automatic updates any more
        return mock.Mock(returncode=0 if ok else 1, stdout='', stderr='' if ok else 'pb: open /tmp/pb-missing: no such file or directory')


def run(machine, scenario, pid='42', health=None):
    machine.health = []

    def settled(expected, seconds, **kw):
        machine.health.append(expected)
        if health:
            raise AssertionError(health)
        return {'version': expected, 'components': ['hostd', 'updated', 'carrier'], 'doctor': 'healthy'}
    with mock.patch.object(update_scheduled, 'clock', machine), \
            mock.patch.object(h.update_health, 'settled', settled), \
            mock.patch.object(h, 'updater_pid', lambda: pid), \
            mock.patch.object(update_scheduled.p, 'invoke', machine.invoke), \
            mock.patch.object(h.p, 'command_result', machine.command), \
            mock.patch.object(h, 'updater', machine.updater), \
            mock.patch.object(h.shutil, 'copy2', lambda *a: None), \
            mock.patch.object(h.os.path, 'realpath', lambda p: p), \
            mock.patch.object(h.winhost, 'device', lambda: None), \
            mock.patch.object(h, 'relabelled', []), \
            mock.patch('sys.argv', ['x', '--scenario', scenario, '--installed', machine.version, '--latest', machine.latest]), \
            mock.patch('builtins.print') as printed:
        h.main()
    return json.loads(printed.call_args[0][0])


class UpdateWindowHold(unittest.TestCase):
    def test_hold_stays_on_a_through_restart_and_both_manual_updates(self):
        machine = Machine()
        result = run(machine, 'hold')
        self.assertEqual((result['status'], result['stayed_on']), ('PASS', '2026.11.27.0'))
        self.assertEqual(machine.actions, ['restart'])
        self.assertEqual(len(machine.froms), 2)
        self.assertTrue(any(s['phase'] == 'custom after a same-version pb update --from' for s in result['samples']))
        self.assertEqual(machine.local_time, (datetime.datetime(2026, 11, 27, 9, 14)).strftime('%H:%M'))  # not restored: custom

    def test_hold_fails_when_the_machine_installs_b_after_a_restart(self):
        with self.assertRaisesRegex(AssertionError, 'outside the window'):
            run(Machine(eager=True), 'hold')

    def test_missed_window_catches_up_on_wake(self):
        machine = Machine()
        result = run(machine, 'missed')
        self.assertEqual(result['status'], 'PASS')
        self.assertEqual(machine.actions, ['freeze', 'wake'])
        self.assertEqual(machine.version, machine.latest)
        self.assertEqual(machine.local_time, '04:00')  # restored

    def test_missed_window_fails_when_nothing_installs(self):
        machine = Machine()
        def asleep_forever(action, pid=None):  # freezes and wakes, but never catches up
            machine.actions.append(action)
            machine.frozen = action == 'freeze'
            return '42'
        machine.updater = asleep_forever
        with mock.patch.object(update_scheduled, 'READY_SECONDS', 60), mock.patch.object(update_scheduled, 'ACTIVATION_SECONDS', 60):
            with self.assertRaisesRegex(AssertionError, 'missed window'):
                run(machine, 'missed')


    def test_every_update_is_followed_by_a_health_check_on_its_version(self):
        machine = Machine()
        run(machine, 'hold')
        self.assertEqual(machine.health, ['2026.11.27.0'])
        machine = Machine()
        run(machine, 'missed')
        self.assertEqual(machine.health, ['2026.11.27.3'])

    def test_a_component_on_the_old_version_after_the_catch_up_fails_the_row(self):
        with self.assertRaisesRegex(AssertionError, 'guard on 2026.11.27.0'):
            run(Machine(), 'missed', health='machine_guard_version warning: guard on 2026.11.27.0')

    def test_missed_window_fails_when_the_updater_was_restarted_while_frozen(self):
        with self.assertRaisesRegex(AssertionError, 'restarted while frozen'):
            run(Machine(), 'missed', pid='43')

    def test_hold_fails_when_the_failed_update_fails_for_another_reason(self):
        machine = Machine()
        machine.command = lambda binary, *args, **kw: mock.Mock(returncode=2, stdout='', stderr='unknown flag --from')
        with self.assertRaisesRegex(AssertionError, 'another reason'):
            run(machine, 'hold')

    def test_hold_checks_the_official_install_up_to_the_minute_before_the_window(self):
        result = run(Machine(), 'hold')
        official = [s for s in result['samples'] if s['phase'].startswith('official')]
        self.assertTrue(official and official[-1]['at'] >= '2026-11-27T09:12')


if __name__ == '__main__':
    unittest.main()
