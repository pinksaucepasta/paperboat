"""Captured binary output remains private and never hides the command failure."""
import json
from pathlib import Path
import stat
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from pairing_lifecycle import CommandFailed, command_diagnostic


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


class ConfigSurvivalUpdateFailure(unittest.TestCase):
    def test_failed_update_keeps_pb_output_in_private_diagnostics(self):
        import config_survival
        failed = subprocess.CompletedProcess(['pb'], 1, stdout=b'', stderr='pb: update refused: fixture-reason')
        with tempfile.TemporaryDirectory() as directory, \
                patch.object(config_survival.winhost, 'device', return_value=None), \
                patch.object(config_survival.winhost, 'copy_file'), \
                patch.object(config_survival.winhost, 'workdir') as workdir, \
                patch.object(config_survival, 'registration', return_value={'machine_id': 'm'}), \
                patch.object(config_survival, 'phase', return_value={}), \
                patch.object(config_survival, 'command_result', return_value=failed), \
                patch('pairing_lifecycle.tempfile.gettempdir', return_value=directory):
            workdir.return_value.__enter__.return_value = Path(directory)
            workdir.return_value.__exit__.return_value = False
            with self.assertRaises(CommandFailed) as caught:
                config_survival.main()
            record = json.loads(caught.exception.diagnostic_path.read_text())
            self.assertIn('pb: update refused: fixture-reason', record['stderr'])
            self.assertEqual([str(config_survival.PB), 'update', '--from', str(Path(directory) / 'pb')], record['argv'])


if __name__ == '__main__':
    unittest.main()
