import contextlib
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import transfer_resume as t


class World:
    """A sender (this machine) and a receiver whose clock runs `skew` seconds ahead. The send shows one in-flight
    progress event and then nothing more: it is over before the next one (the LP6 false FAIL, 28 Nov)."""

    def __init__(self, restarts=True, publish_after=12.0, exit_code=0, progress=True, skew=0.0):
        self.now, self.skew = 1000.0, skew
        self.restarts, self.publish_after, self.exit_code, self.progress = restarts, publish_after, exit_code, progress
        self.daemon_start = {'receiver': 100.0, 'sender': 100.0}  # on each host's own clock
        self.asked_at = None  # sender clock
        self.dropped = []

    # time, as transfer_resume uses it
    def time(self):
        self.now += 0.05
        return self.now

    monotonic = time

    def sleep(self, seconds):
        self.now += seconds

    # the send job
    def stderr(self):
        events = [{'event': 'transfer_progress', 'bytes_sent': 3, 'total_bytes': 10}] if self.progress else []
        return '\n'.join(map(json.dumps, events)).encode()

    def poll(self):  # over as soon as it has been interrupted, before another progress event
        return None if self.progress and self.asked_at is None else self.exit_code

    def wait(self, timeout):
        return self.exit_code

    def stdout(self):
        if self.exit_code:
            return json.dumps({'error': {'code': 'transfer_failed', 'support_reference': 'support_fixture'}}).encode()
        return json.dumps({'data': {'transfers': [{'transfer_id': 'transfer_%d' % n} for n in range(t.FILES)]}}).encode()

    def kill(self):
        pass

    close = kill

    def restart(self, host, clock):
        if self.asked_at is None:
            self.asked_at = self.now
        if self.restarts:
            self.daemon_start[host] = clock + 1.5  # sleep 1, then the restart

    def published(self):  # receiver clock
        return (self.asked_at if self.asked_at is not None else self.now) + self.publish_after + self.skew

    def exec(self, binary, verb, target, dashes, *argv, **kwargs):
        script = argv[-1]
        if 'service restart' in script:
            issued = self.time() + self.skew
            self.restart('receiver', issued)
            return str(issued).encode(), b''
        if script == t.CLOCK[0]:
            return str(self.time() + self.skew).encode(), b''
        if script == t.DAEMON_STARTED[0]:
            return b'%f 0.05' % self.daemon_start['receiver'], b''
        if '.published' in script:
            return '\n'.join([str(self.published() - 2)] * (t.FILES - 1) + [str(self.published())]).encode(), b''
        if 'PB_RESUME_VERIFIED' in script:
            return b'PB_RESUME_VERIFIED\n', b''
        return b'', b''  # inbox cleanup

    def local(self, command, **kwargs):
        out = ''
        if command[0] == sys.executable and command[-1] == t.CLOCK[0]:
            out = str(self.time())
        elif command[0] == sys.executable and command[-1] == t.DAEMON_STARTED[0]:
            out = '%f 0.05' % self.daemon_start['sender']
        elif command[:2] == ['sh', '-c']:
            self.restart('sender', self.time())
        elif command[:3] == ['sudo', '-n', 'iptables']:
            self.dropped.append(command[3])
            if command[3] == '-I':
                self.asked_at = self.now
        else:
            raise AssertionError(command)
        return subprocess.CompletedProcess(command, 0, out, '')


def run_check(world, interrupt='receiver'):
    @contextlib.contextmanager
    def workdir(prefix):
        with tempfile.TemporaryDirectory() as directory:
            yield Path(directory)
    fake_subprocess = mock.Mock(run=world.local, DEVNULL=subprocess.DEVNULL, TimeoutExpired=subprocess.TimeoutExpired)
    output = io.StringIO()
    with mock.patch.object(t, 'time', world), mock.patch.object(t, 'subprocess', fake_subprocess), \
            mock.patch.object(t, 'run', world.exec), mock.patch.object(t, 'registration', lambda: 'same'), \
            mock.patch.object(t, 'pilot_rate', lambda *a: 1e6), mock.patch.object(t.winhost, 'device', lambda: None), \
            mock.patch.object(t.winhost, 'pb_path', lambda: Path('/fixture/pb')), \
            mock.patch.object(t.winhost, 'workdir', workdir), mock.patch.object(t.winhost, 'job', lambda *a: world), \
            mock.patch.object(t.winhost, 'write_random', lambda path, size: 'f' * 64), \
            mock.patch.object(t.windows_target, 'is_windows', lambda *a: False), \
            mock.patch.object(sys, 'argv', ['transfer_resume.py', 'peer', '--interrupt', interrupt]), \
            contextlib.redirect_stdout(output):
        t.main()
    return json.loads(output.getvalue().splitlines()[-1])


class TransferResume(unittest.TestCase):
    def test_a_send_that_resumed_and_finished_before_another_progress_event_passes(self):
        for interrupt in ('receiver', 'sender', 'network'):
            with self.subTest(interrupt):
                world = World(publish_after=25.0)
                receipt = run_check(world, interrupt)
                self.assertEqual(('PASS', interrupt, True),
                                 (receipt['status'], receipt['interrupt'], receipt['enrollment_unchanged']))
                self.assertGreater(receipt['published_after_interruption_s'], 0)
        self.assertEqual(['-I', '-D'], world.dropped)

    def test_a_restart_that_never_happened_fails(self):
        for interrupt in ('receiver', 'sender'):
            with self.subTest(interrupt), self.assertRaisesRegex(RuntimeError, 'daemon restart never happened'):
                run_check(World(restarts=False), interrupt)

    def test_an_interruption_after_the_last_publish_fails(self):
        for interrupt, publish_after in (('receiver', 1.0), ('sender', 1.0), ('network', t.DROP_SECONDS - 5)):
            with self.subTest(interrupt), self.assertRaisesRegex(RuntimeError, 'did not land while the batch was in flight'):
                run_check(World(publish_after=publish_after), interrupt)

    def test_a_send_that_fails_or_ends_before_the_interruption_fails(self):
        with self.assertRaisesRegex(RuntimeError, r'did not finish by itself \(exit 1.*transfer_failed support_fixture'):
            run_check(World(exit_code=1))
        with self.assertRaisesRegex(RuntimeError, 'ended before a progress event showed it in flight'):
            run_check(World(progress=False))

    def test_another_hosts_clock_is_allowed_for(self):
        # The receiver is 90 s ahead: its publish times are compared with the sender's interruption on its own clock.
        for interrupt in ('sender', 'network'):
            with self.subTest(interrupt):
                self.assertEqual('PASS', run_check(World(publish_after=25.0, skew=90.0), interrupt)['status'])
                with self.assertRaisesRegex(RuntimeError, 'did not land'):
                    run_check(World(publish_after=1.0, skew=90.0), interrupt)
        with self.assertRaisesRegex(RuntimeError, 'did not land'):  # 90 s behind, published before the restart
            run_check(World(publish_after=1.0, skew=-90.0), 'sender')

    def test_clock_ahead_is_an_upper_bound_through_a_third_machine(self):
        ticks = iter(range(0, 1000, 2))  # each read takes 2 s of this machine's time
        now = lambda: float(next(ticks))
        here = [0.0]

        def clock(ahead):
            def read():
                here[0] += 4
                return here[0] - 4 + 1 + ahead  # read half way through the 2 s round trip
            return read
        self.assertGreaterEqual(t.clock_ahead(clock(30.0), now=now), 30.0)
        ticks = iter(range(0, 1000, 2))
        here[0] = 0.0
        with_source = t.clock_ahead(clock(30.0), clock(10.0), samples=1, now=lambda: float(next(ticks)))
        self.assertGreaterEqual(with_source, 20.0)

    def test_publish_times_need_every_transfer(self):
        with self.assertRaisesRegex(RuntimeError, '1 publish times for 2 transfers'):
            t.last_published('12.5\n', 2, False)
        self.assertEqual(13.5, t.last_published('13500\r\n12000\r\n', 2, True))
        with self.assertRaisesRegex(RuntimeError, 'no usable transfer ids'):
            t.published_scripts(["transfer_1'; rm"])

    def test_the_real_proof_scripts_run(self):
        started = subprocess.run([sys.executable, '-c', t.CLOCK[0]], capture_output=True, text=True, check=True)
        self.assertGreater(float(started.stdout), 1e9)
        for script in (t.DAEMON_STARTED[0], t.published_scripts(['transfer_ab-1'])[0]):
            compile(script, 'proof', 'exec')


if __name__ == '__main__':
    unittest.main()
