"""A failed fixture child must retain its actual private error, even before PID output."""
import json
from pathlib import Path
import stat
import tempfile
import unittest

from exec_cancel import one


class ExecCancellationDiagnostics(unittest.TestCase):
    def test_startup_failure_retains_child_error_in_owner_only_file(self):
        with tempfile.TemporaryDirectory() as directory:
            binary = Path(directory) / 'pb-fixture'
            binary.write_text('#!/usr/bin/env python3\nimport json\n'
                              'print(json.dumps({"event":"failed","error_code":"fixture_start_failed"}))\n'
                              'raise SystemExit(255)\n')
            binary.chmod(0o700)
            with self.assertRaises(RuntimeError) as raised:
                one(binary, 'fixture-target', 'timeout')
            notes = getattr(raised.exception, '__notes__', [])
            paths = [Path(note.removeprefix('Private command diagnostics: '))
                     for note in notes if note.startswith('Private command diagnostics: ')]
            self.assertEqual(len(paths), 1)
            self.addCleanup(paths[0].unlink, missing_ok=True)
            receipt = json.loads(paths[0].read_text())
            self.assertEqual(json.loads(receipt['stdout'])['error_code'], 'fixture_start_failed')
            self.assertEqual(receipt['argv'][0], str(binary))
            self.assertEqual(stat.S_IMODE(paths[0].stat().st_mode), 0o600)


if __name__ == '__main__':
    unittest.main()
