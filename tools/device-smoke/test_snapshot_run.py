"""A running smoke source window survives edits to its original files."""
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest

RUNNER = Path(__file__).with_name('snapshot_run.py')


class SourceWindowTests(unittest.TestCase):
    def test_shell_and_staged_helpers_remain_the_started_revision(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            script, helper = root / 'smoke.sh', root / 'fixture.py'
            helper.write_text('original helper\n')
            prefix = 'printf "READY\\n"\nread -r signal\n' + '# source padding\n' * 10000
            script.write_text(prefix + 'cat "$PAPERBOAT_SMOKE_FIXTURES" > "$1"\n'
                              'printf "ORIGINAL\\n"\n')
            archive = root / 'captured.tar.gz'
            child = subprocess.Popen([sys.executable, str(RUNNER), str(script), str(archive)],
                                     stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                     stderr=subprocess.PIPE, text=True)
            try:
                self.assertEqual(child.stdout.readline(), 'READY\n')
                script.write_text(prefix + 'exit 77\n')
                helper.write_text('replacement helper\n')
                output, error = child.communicate('continue\n', timeout=10)
                self.assertEqual(child.returncode, 0, error)
                self.assertEqual(output, 'ORIGINAL\n')
                with tarfile.open(archive, 'r:gz') as bundle:
                    self.assertEqual(bundle.extractfile('fixture.py').read(), b'original helper\n')
            finally:
                if child.poll() is None:
                    child.kill()
                    child.communicate(timeout=5)


if __name__ == '__main__':
    unittest.main()
