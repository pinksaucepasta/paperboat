import os
from pathlib import Path
import sys
import tempfile
import threading
import time
import unittest

from held_terminal_gap import collect_markers
from terminal_lifecycle import Terminal


class HeldTerminalGapTest(unittest.TestCase):
    def test_slow_checkpoint_storage_cannot_delay_marker_observation(self):
        # An unopened FIFO models the blocking write seen on the Pi. The real
        # PTY must still collect every marker before any persistence is attempted.
        child = ("import sys,time\n"
                 "for i in range(3):\n"
                 " print('PB_GAP_%d_123_1000.%06d' % (i,i),flush=True)\n"
                 " time.sleep(.05)\n"
                 "time.sleep(5)\n")
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            os.mkfifo(root / 'markers.json')
            terminal = Terminal([sys.executable, '-c', child])
            # Opening the writer really blocks until the storage reader starts.
            def drain():
                time.sleep(1)
                while True:
                    try:
                        with (root / 'markers.json').open('rb') as stream:
                            stream.read()
                    except FileNotFoundError:
                        return
            threading.Thread(target=drain, daemon=True).start()
            try:
                rows = collect_markers(terminal, 123, 3, root)
                self.assertEqual([r['i'] for r in rows], [0, 1, 2])
                self.assertLess((rows[-1]['ns'] - rows[0]['ns']) / 1e9, .5,
                                'marker timestamps include checkpoint storage latency')
            finally:
                terminal.close()



if __name__ == '__main__':
    unittest.main()
