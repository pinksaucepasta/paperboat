"""Captured binary output remains private and never hides the command failure."""
import json
from pathlib import Path
import stat
import tempfile
import unittest
from unittest.mock import patch

from pairing_lifecycle import command_diagnostic


class CommandDiagnostics(unittest.TestCase):
    def test_captured_output_buffers_are_saved_in_an_owner_only_receipt(self):
        for value in [b'captured\xff', bytearray(b'captured\xff'), 'captured\ufffd']:
            with self.subTest(kind=type(value).__name__), tempfile.TemporaryDirectory() as directory:
                with patch('pairing_lifecycle.tempfile.gettempdir', return_value=directory):
                    receipt = command_diagnostic(['pb', 'synthetic'], value, b'fixture error', timeout=3)
                self.assertEqual(Path(directory), receipt.parent)
                self.assertEqual(0o600, stat.S_IMODE(receipt.stat().st_mode))
                record = json.loads(receipt.read_text())
                self.assertEqual('captured\ufffd', record['stdout'])
                self.assertEqual('fixture error', record['stderr'])
                self.assertEqual(['pb', 'synthetic'], record['argv'])
                self.assertEqual(3, record['timeout'])


if __name__ == '__main__':
    unittest.main()
