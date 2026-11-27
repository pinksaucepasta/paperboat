"""net_blip.py: the row cuts the CLIENT, and waits for doctor to stop saying healthy up to a deadline."""
import contextlib
import io
import json
import subprocess
import sys
import unittest
from unittest.mock import MagicMock, patch

import net_blip
import winhost


class Clock:
    def __init__(self):
        self.now = 1000.0

    def monotonic(self):
        return self.now

    def sleep(self, seconds):
        self.now += seconds


class NetBlipTests(unittest.TestCase):
    def doctor(self, clock, flips_at):
        """A doctor that takes 5 s per run and reports unhealthy from `flips_at` seconds on."""
        start = clock.now

        def answer():
            began = clock.now - start
            clock.now += 5
            return ('unhealthy', ['authentication=fail']) if began >= flips_at else ('healthy', [])
        return answer

    def test_doctor_is_polled_until_it_stops_saying_healthy(self):
        clock = Clock()
        with patch.object(net_blip, 'time', clock), patch.object(net_blip, 'doctor_report', self.doctor(clock, 150)):
            overall, at, samples = net_blip.doctor_flip(clock.now, 180)
        self.assertEqual(('unhealthy', 155.0), (overall, at))  # healthy at 90 s is not the verdict
        self.assertEqual([[5.0, 'healthy', []], [155.0, 'unhealthy', ['authentication=fail']]], [samples[0], samples[-1]])

    def test_doctor_healthy_past_the_deadline_never_flips(self):
        clock = Clock()
        with patch.object(net_blip, 'time', clock), patch.object(net_blip, 'doctor_report', self.doctor(clock, 181)):
            overall, at, _ = net_blip.doctor_flip(clock.now, 180)
        self.assertEqual(('healthy', None), (overall, at))

    def test_an_answer_long_after_the_deadline_is_not_a_flip(self):
        for took, flipped in ((25, 200.0), (100, None)):  # started at 175 s
            clock = Clock()
            def slow(clock=clock, took=took):
                clock.now += took
                return 'unhealthy', []
            started = clock.now - 175
            with self.subTest(took=took), patch.object(net_blip, 'time', clock), patch.object(net_blip, 'doctor_report', slow):
                self.assertEqual(flipped, net_blip.doctor_flip(started, 180)[1])

    def test_a_doctor_that_gives_no_report_is_not_a_flip(self):
        for failure in (subprocess.TimeoutExpired('pb', 120), None):
            with self.subTest(failure), patch.object(net_blip.winhost, 'run_pb', side_effect=failure,
                                                      return_value=subprocess.CompletedProcess([], 1, b'no daemon', b'')):
                self.assertEqual(('', []), net_blip.doctor_report())
        clock = Clock()
        with patch.object(net_blip, 'time', clock), patch.object(net_blip, 'doctor_report', lambda: ('', [])):
            self.assertIsNone(net_blip.doctor_flip(clock.now, 180)[1])

    def row(self, device, proof_error=None, flips_at=150):
        clock, events = Clock(), []
        cut = MagicMock()
        cut.release.side_effect = lambda: events.append('release')
        cut.proof.side_effect = proof_error or (lambda: 'sent=0 captured=40 dropped=9')
        if device:
            device.cut_client.side_effect = lambda seconds: events.append('cut client %d' % seconds) or cut
            device.ps.return_value = (0, '2026-01-01T00:00:00Z')
        down = lambda: 'release' not in events and 'release peer' not in events
        out = io.StringIO()
        with patch.object(net_blip, 'time', clock), patch.object(net_blip.network_drop, 'DEVICE', device), \
                patch.object(winhost, 'PEER_CUT', ['host_cut.sh', 'peer', '1001']), \
                patch.object(winhost, 'peer_cut', side_effect=AssertionError('the row must not cut the peer') if device else None), \
                patch.object(net_blip.network_drop, 'cut', side_effect=lambda s: events.append('cut local %d' % s) or ('block', None)), \
                patch.object(net_blip.network_drop, 'release', side_effect=lambda b: events.append('release')), \
                patch.object(net_blip.network_drop, 'sudo', return_value=subprocess.CompletedProcess([], 0, '', '')), \
                patch.object(net_blip, 'exec_ok', lambda target, timeout=25: 'release' in events or not any(e.startswith('cut') for e in events)), \
                patch.object(net_blip, 'doctor_report', self.doctor(clock, flips_at)), \
                patch.object(net_blip, 'attach', lambda target, name, deadline: clock.now), \
                patch.object(net_blip, 'cleanup'), patch.object(sys, 'argv', ['net_blip.py', 'peer-1']), \
                contextlib.redirect_stdout(out):
            code = net_blip.main()
        return code, json.loads(out.getvalue()), events

    def test_a_windows_client_is_cut_itself_even_with_a_peer_cut_configured(self):
        code, report, events = self.row(MagicMock())
        self.assertEqual((0, 'PASS'), (code, report['status']), report)
        self.assertEqual(['cut client 480', 'release'], events)
        self.assertEqual('sent=0 captured=40 dropped=9', report['cut'])
        self.assertEqual(155.0, report['doctor_flipped_after_s'])

    def test_a_windows_cut_that_blocked_nothing_fails_the_row(self):
        code, report, _ = self.row(MagicMock(), proof_error=RuntimeError('not cut: sent=4 captured=40 dropped=9'))
        self.assertEqual((1, 'FAIL', None), (code, report['status'], report['cut']))
        self.assertIn('not cut', report['error'])

    def test_a_unix_client_keeps_its_own_cut_and_the_same_deadline(self):
        code, report, events = self.row(None)
        self.assertEqual((0, 'PASS'), (code, report['status']), report)
        self.assertEqual(['cut local 480', 'release'], events)
        self.assertNotIn('cut', report)
        code, report, _ = self.row(None, flips_at=10_000)
        self.assertEqual((1, 'FAIL', None), (code, report['status'], report['doctor_flipped_after_s']))


if __name__ == '__main__':
    unittest.main()
