"""Backup publication, retention and failure cleanup at the executable script boundary (database host)."""
import os
from pathlib import Path
import subprocess
import tempfile
import time
import unittest


SCRIPT = Path(__file__).resolve().parents[2] / "deploy/backup-database.sh"
# runuser stands in for `runuser -u postgres -- pg_dump ... -f FILE`: it writes the fixture to FILE (or fails).
RUNUSER = '#!/bin/sh\nwhile [ "$1" != -f ]; do shift; done\nprintf dump-fixture > "$2"\nexit %d\n'


class BackupPublicationTest(unittest.TestCase):
    def test_success_publishes_protected_dump_and_drops_week_old_ones(self):
        self.check_backup(False)

    def test_failed_dump_keeps_previous_and_removes_partial(self):
        self.check_backup(True)

    def check_backup(self, fail):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "bin").mkdir()
            backups = root / "backups"
            backups.mkdir()
            previous = backups / "database-20261120T000000Z.dump"
            previous.write_bytes(b"previous")
            expired = backups / "database-20261101T000000Z.dump"
            expired.write_bytes(b"expired")
            week_old = time.time() - 8 * 86400
            os.utime(expired, (week_old, week_old))
            for name, body in (("runuser", RUNUSER % (1 if fail else 0)), ("chown", "#!/bin/sh\nexit 0\n")):
                (root / "bin" / name).write_text(body)
                (root / "bin" / name).chmod(0o755)
            env = dict(os.environ, PATH=f"{root / 'bin'}:{os.environ['PATH']}", PAPERBOAT_BACKUP_DIR=str(backups))
            result = subprocess.run(["sh", str(SCRIPT)], env=env, capture_output=True, timeout=10)
            self.assertEqual(previous.read_bytes(), b"previous")
            self.assertEqual(list(backups.glob(".database.*")), [])
            published = sorted(set(backups.glob("database-*.dump")) - {previous, expired})
            if fail:
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(published, [])
                self.assertTrue(expired.exists(), "a failed backup must not drop old dumps")
            else:
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(len(published), 1)
                self.assertEqual(published[0].read_bytes(), b"dump-fixture")
                self.assertEqual(published[0].stat().st_mode & 0o777, 0o600)
                self.assertEqual(backups.stat().st_mode & 0o777, 0o700)
                self.assertFalse(expired.exists())


if __name__ == "__main__":
    unittest.main()
