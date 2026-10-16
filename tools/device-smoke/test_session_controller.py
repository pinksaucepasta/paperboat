"""The shared controller admits only the expected phase and signals its exact gate."""
import json
from pathlib import Path
import shlex
import stat
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('session_controller.py')


class PhaseAdmissionTests(unittest.TestCase):
    def test_exact_phase_gate_is_exclusive_for_terminal_and_preview(self):
        for state in ['terminal_attach_required', 'preview_protocol_required']:
            with self.subTest(state=state), tempfile.TemporaryDirectory() as directory:
                gate = Path(directory) / 'accepted'
                refused = Path(directory) / 'refused'
                phase = json.dumps({'state': state, 'gate': str(gate), 'failure_gate': str(refused)})
                args = [sys.executable, str(SCRIPT), 'signal', '--phase', phase, '--success', '1']
                if state != 'terminal_attach_required':
                    args += ['--expected-state', state]
                result = subprocess.run(args, capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode, 0, result.stderr)
                command = shlex.split(result.stdout)
                completed = subprocess.run(command, capture_output=True, timeout=5)
                self.assertEqual(completed.returncode, 0)
                self.assertTrue(gate.is_file())
                self.assertEqual(stat.S_IMODE(gate.stat().st_mode), 0o600)
                self.assertFalse(refused.exists())
                gate.write_bytes(b'original owner')
                repeated = subprocess.run(command, capture_output=True, timeout=5)
                self.assertNotEqual(repeated.returncode, 0)
                self.assertEqual(gate.read_bytes(), b'original owner')
                mismatch = subprocess.run(args + ['--expected-state', 'terminal_attach_required' if state != 'terminal_attach_required' else 'preview_protocol_required'], capture_output=True, timeout=5)
                self.assertNotEqual(mismatch.returncode, 0)
                self.assertEqual(mismatch.stdout, b'')


if __name__ == '__main__':
    unittest.main()
