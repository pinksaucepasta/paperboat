"""A running smoke source window survives edits to its original files."""
from pathlib import Path
import os
import subprocess
import sys
import tarfile
import tempfile
import unittest

RUNNER = Path(__file__).with_name('snapshot_run.py')
# Same results-directory rule as smoke.sh: SMOKE_RESULTS_DIR when set, else the source's results/.
SHARD = '''RESULTS_DIR=${SMOKE_RESULTS_DIR:-"$PAPERBOAT_SMOKE_SOURCE_DIR/results"}
mkdir -p "$RESULTS_DIR"
sleep 1
printf 'receipt\\n' > "$RESULTS_DIR/$1.json"
'''


def run_shards(root, shards):
    """Run the snapshot runner concurrently, one (name, results directory or None) pair per shard."""
    (root / 'src').mkdir()
    script, settings = root / 'src' / 'smoke.sh', root / 'targets.json'
    script.write_text(SHARD)
    (root / 'src' / 'helper.py').write_text('# fixture\n')
    settings.write_text('{}')
    children = []
    for name, results in shards:
        environment = {key: value for key, value in os.environ.items() if key != 'SMOKE_RESULTS_DIR'}
        environment.update(PB_DEV_TARGETS_FILE=str(settings), TMPDIR=str(root / 'tmp'))
        if results:
            environment['SMOKE_RESULTS_DIR'] = str(results)
        children.append(subprocess.Popen([sys.executable, str(RUNNER), str(script), name], env=environment,
                                         stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True))
    for child in children:
        output, error = child.communicate(timeout=60)
        if child.returncode:
            raise AssertionError(error)


def written_files(root):
    return sorted(str(path.relative_to(root)) for path in root.rglob('*') if path.is_file())


class ResultsDirectoryTests(unittest.TestCase):
    def test_concurrent_shards_write_only_their_own_directories(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'tmp').mkdir()
            run_shards(root, [('shard-a', root / 'out' / 'a'), ('shard-b', root / 'out' / 'b')])
            # Inputs, one receipt per shard, and nothing left in the frozen temp dir or the source's results/.
            self.assertEqual(written_files(root), ['out/a/shard-a.json', 'out/b/shard-b.json',
                                                   'src/helper.py', 'src/smoke.sh', 'targets.json'])
            self.assertEqual((root / 'out' / 'a' / 'shard-a.json').read_text(), 'receipt\n')
            self.assertEqual(list((root / 'tmp').iterdir()), [])

    def test_unset_directory_keeps_the_source_results_default(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'tmp').mkdir()
            run_shards(root, [('shard-x', None)])
            self.assertEqual(written_files(root), ['src/helper.py', 'src/results/shard-x.json', 'src/smoke.sh',
                                                   'targets.json'])


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
