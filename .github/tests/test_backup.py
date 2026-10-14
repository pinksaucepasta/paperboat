"""Backup publication and failure cleanup at the executable script boundary."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[2] / "deploy/backup-database.sh"


class BackupPublicationTest(unittest.TestCase):
    def test_success_publishes_protected_dump(self):
        self.check_backup(False)

    def test_failed_dump_keeps_previous_and_removes_partial(self):
        self.check_backup(True)

    def check_backup(self, fail):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            shutil.copy2(SCRIPT, root / "backup-database.sh")
            (root / "bin").mkdir()
            (root / "backups").mkdir()
            previous = root / "backups/previous.dump"
            previous.write_bytes(b"previous")
            docker = root / "bin/docker"
            docker.write_text("#!/bin/sh\nprintf 'dump-fixture'\nexit " + ("1\n" if fail else "0\n"))
            docker.chmod(0o755)
            env = dict(os.environ, PATH=f"{root / 'bin'}:{os.environ['PATH']}")
            result = subprocess.run(["sh", str(root / "backup-database.sh")], env=env, capture_output=True, timeout=10)
            self.assertEqual(previous.read_bytes(), b"previous")
            self.assertEqual(list((root / "backups").glob(".database.*")), [])
            published = list((root / "backups").glob("database-*.dump"))
            if fail:
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(published, [])
            else:
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(len(published), 1)
                self.assertEqual(published[0].read_bytes(), b"dump-fixture")
                self.assertEqual(published[0].stat().st_mode & 0o777, 0o600)
                self.assertEqual((root / "backups").stat().st_mode & 0o777, 0o700)


if __name__ == "__main__":
    unittest.main()
