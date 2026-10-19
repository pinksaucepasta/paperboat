"""The self-host installer publication scripts, run against a temporary release root."""
from pathlib import Path
import subprocess
import unittest

DEPLOY = Path(__file__).resolve().parents[2] / "helper" / "deploy"


class SelfhostPublicationTest(unittest.TestCase):
    def test_publisher_is_idempotent_immutable_bounded_and_atomic(self):
        result = subprocess.run(["bash", str(DEPLOY / "test-publish-selfhost-origin.sh")],
                                capture_output=True, text=True, timeout=120)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
