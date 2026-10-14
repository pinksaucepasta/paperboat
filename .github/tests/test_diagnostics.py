"""Exercise credential refusal at the diagnostics process boundary."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


class DiagnosticsCredentialsTest(unittest.TestCase):
    def test_missing_and_empty_credentials_refuse_start_without_disclosure(self):
        with tempfile.TemporaryDirectory() as directory:
            empty = Path(directory) / "empty"
            empty.write_text("")
            marker = Path(directory) / "marker"
            marker.write_text("fixture-private-value")
            for values in [{}, {"MINIO_ROOT_USER_FILE": str(marker),
                                "MINIO_ROOT_PASSWORD_FILE": str(empty)}]:
                result = subprocess.run(["sh", str(ROOT / "deploy/diagnostics-entrypoint.sh")],
                                        env={"PATH": os.environ["PATH"], **values},
                                        capture_output=True, text=True, timeout=5)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn(marker.read_text(), result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
