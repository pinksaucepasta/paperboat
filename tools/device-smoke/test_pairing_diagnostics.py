import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import unittest

from pairing_lifecycle import invoke


class CommandDiagnostics(unittest.TestCase):
    def test_timeout_keeps_both_output_channels_in_private_receipt(self):
        child = ("import sys,time; print('fixture-out',flush=True); "
                 "print('fixture-error',file=sys.stderr,flush=True); "
                 "print('fixture-secret',flush=True); time.sleep(60)")
        for terminal in (False, True):
            with self.subTest(terminal=terminal):
                with self.assertRaises(subprocess.TimeoutExpired) as raised:
                    invoke(sys.executable, '-c', child, '--token=fixture-secret',
                           timeout=2, raw=True, terminal=terminal,
                           password='fixture-secret' if terminal else None)
                diagnostic = getattr(raised.exception, 'diagnostic_path', None)
                self.assertIsNotNone(diagnostic, 'timeout discarded its actual output')
                try:
                    receipt = Path(diagnostic)
                    self.assertNotIn('fixture-secret', str(raised.exception))
                    self.assertEqual(0o600, stat.S_IMODE(receipt.stat().st_mode))
                    self.assertEqual(os.getuid(), receipt.stat().st_uid)
                    saved = json.loads(receipt.read_text())
                    output = saved['stdout'] + saved['stderr']
                    self.assertIn('fixture-out', output)
                    self.assertIn('fixture-error', output)
                    if terminal:
                        self.assertNotIn('fixture-secret', output)
                        self.assertIn('[redacted]', output)
                    self.assertEqual(2, saved['timeout'])
                    self.assertIn('--token=fixture-secret', saved['argv'])
                finally:
                    Path(diagnostic).unlink(missing_ok=True)


if __name__ == '__main__':
    unittest.main()
