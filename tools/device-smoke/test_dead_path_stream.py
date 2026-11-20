import csv
import tempfile
import threading
import unittest

import dead_path_stream as dps
import winhost


class TraceTest(unittest.TestCase):
    def test_a_slow_cut_leaves_the_arrivals_and_the_path_samples_that_explain_it(self):
        blocked = 1000.0
        arrivals = [(999.9, b'0'), (1016.1, b'1'), (1016.2, b'2')]  # cut-16s.md: nothing for 16.1 s
        direct = {'selected_path': 'direct', 'home_region': 'in'}
        samples = [(999.5, direct), (1001.5, direct), (1007.0, dict(direct, selected_path='wss')), (1010.0, {'error': 'status timed out'}),
                   (1012.0, dict(direct, selected_path='wss', home_region='us'))]
        with tempfile.TemporaryDirectory() as directory:
            summary = dps.write_trace(directory, blocked, arrivals, samples)
            with open(directory + '/arrivals.csv') as handle:
                rows = list(csv.reader(handle))
            with open(directory + '/status.csv') as handle:
                status = list(csv.reader(handle))
        self.assertEqual([['t_ms', 'line'], ['-100', '0'], ['16100', '1'], ['16200', '2']], rows)
        self.assertEqual(['t_ms', 'selected_path', 'standby_path', 'relay_region', 'home_region', 'error'], status[0])
        self.assertEqual(['7000', 'wss', '', '', 'in', ''], status[3])
        self.assertEqual({'dir': directory, 'first_line_after_ms': 16100, 'first_non_direct_ms': 7000, 'home_region_changed': True, 'status_errors': 1}, summary)

    def test_the_sampler_keeps_a_failed_reading_and_stops_when_told(self):
        stop, samples, calls = threading.Event(), [], []

        def sample(peer):
            calls.append(peer)
            if len(calls) == 2:
                raise RuntimeError('pb status failed')
            if len(calls) == 3:
                stop.set()
            return {'selected_path': 'direct'}
        dps.sample_status('laptop', stop, samples, interval=0.01, sample=sample)
        self.assertEqual(['direct', 'pb status failed'], [r.get('selected_path') or r['error'] for _, r in samples[:2]])
        self.assertEqual(3, len(samples))

    def test_the_stream_client_runs_on_the_windows_device_behind_its_ssh_login(self):
        device = winhost.WindowsDevice(target='user@win', pb_exe=r'C:\Program Files\Paperboat\pb.exe', password='')
        local = dps.stream_argv('laptop', 'echo 1', None)
        self.assertEqual(['exec', 'laptop', '--', 'sh', '-c', 'echo 1'], local[1:])
        argv = dps.stream_argv('laptop', 'i=0; echo $i', device)
        self.assertEqual(['user@win'], [word for word in argv if word.startswith('user@')])
        encoded = argv[-1].rsplit(' ', 1)[1]
        command = __import__('base64').b64decode(encoded).decode('utf-16-le')
        self.assertEqual("& 'C:\\Program Files\\Paperboat\\pb.exe' exec 'laptop' -- sh -c 'i=0; echo $i'", command)


if __name__ == '__main__':
    unittest.main()
