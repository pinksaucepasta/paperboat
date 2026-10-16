"""A new device fixture must arrive with its import dependencies, without test/private files."""
import io
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest

HERE = Path(__file__).resolve().parent


class FixtureArchiveTests(unittest.TestCase):
    def test_new_fixture_imports_its_new_helper_after_compressed_upload(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source, receiver = root / 'source', root / 'receiver'
            source.mkdir(); receiver.mkdir()
            for path in HERE.glob('*.py'):
                if not path.name.startswith('test_'):
                    shutil.copyfile(path, source / path.name)
            (source / 'new_fixture.py').write_text('from new_helper import answer\nassert answer()==42\n')
            (source / 'new_helper.py').write_text('def answer(): return 6*7\n')
            (source / 'test_private.py').write_text('raise RuntimeError("workspace tests must not be uploaded")\n')
            (source / 'private.txt').write_text('private fixture input')
            packed = subprocess.run([sys.executable, str(HERE / 'stage_fixtures.py'), str(source)], capture_output=True, timeout=10)
            self.assertEqual(packed.returncode, 0, packed.stderr)
            with tarfile.open(fileobj=io.BytesIO(packed.stdout), mode='r:gz') as archive:
                archive.extractall(receiver, filter='data')
            result = subprocess.run([sys.executable, str(receiver / 'new_fixture.py')], capture_output=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertFalse((receiver / 'test_private.py').exists())
            self.assertFalse((receiver / 'private.txt').exists())

    def test_refuses_private_symlink_and_oversized_source_before_writing_archive(self):
        for kind in ['symlink', 'oversize']:
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                source = root / 'source'
                source.mkdir()
                if kind == 'symlink':
                    outside = root / 'private.txt'
                    outside.write_text('PRIVATE_ARCHIVE_SENTINEL')
                    (source / 'fixture.py').symlink_to(outside)
                else:
                    (source / 'fixture.py').write_bytes(b'x' * ((256 << 10) + 1))
                result = subprocess.run([sys.executable, str(HERE / 'stage_fixtures.py'), str(source)], capture_output=True, timeout=5)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(result.stdout, b'')
                self.assertNotIn(b'PRIVATE_ARCHIVE_SENTINEL', result.stderr)


if __name__ == '__main__':
    unittest.main()
