import os
import subprocess
import sys
import tempfile
import unittest
from contextlib import ExitStack
from pathlib import Path

from sweep_daemons import stop_unowned_daemon


@unittest.skipUnless(hasattr(os, "pidfd_open"), "Linux process handles required")
class DaemonSweep(unittest.TestCase):
    def test_stops_only_the_daemon_with_a_foreign_config(self):
        with tempfile.TemporaryDirectory() as directory, ExitStack() as cleanup:
            work = Path(directory)
            owned = work / "owned" / "config.json"
            owned.parent.mkdir()
            owned.touch()
            missing = work / "gone" / "config.json"
            (work / "daemon").write_text("import time; time.sleep(30)\n")

            def spawn(*args):
                process = subprocess.Popen([str(work / "pb"), "daemon", *map(str, args)],
                                           executable=sys.executable, cwd=work)
                cleanup.callback(process.wait, timeout=3)
                cleanup.callback(process.kill)
                return process

            stray = spawn("--config", missing)
            managed = spawn("--config=owned/config.json")
            status = spawn("status", "--config", missing)
            for process in (stray, managed, status):
                stop_unowned_daemon(process.pid, str(owned))

            self.assertLess(stray.wait(timeout=2), 0, "daemon using the missing foreign config survived")
            self.assertIsNone(managed.poll(), "daemon using the fixture config was stopped")
            self.assertIsNone(status.poll(), "one-shot daemon command was misidentified")
