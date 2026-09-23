#!/usr/bin/env python3
"""Exercise release builder input rejection and linker propagation without rebuilding pb."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]

class ReleaseTelemetry(unittest.TestCase):
    def test_release_requires_valid_destination_before_invoking_go(self):
        with tempfile.TemporaryDirectory(prefix="pb-release-telemetry-") as temp:
            root = Path(temp)
            compiler = root / "go"
            compiler.write_text("#!/usr/bin/env python3\nimport json,os,sys\nfrom pathlib import Path\nPath(os.environ['PB_TEST_ARGS']).write_text(json.dumps(sys.argv[1:]))\nPath(sys.argv[sys.argv.index('-o')+1]).write_text('fixture')\n")
            compiler.chmod(0o700)
            args_file, output = root / "args.json", root / "pb"
            env = dict(os.environ, PATH=str(root)+os.pathsep+os.environ['PATH'], PB_TEST_ARGS=str(args_file))
            command = [str(ROOT / "tools/build-release-asset.sh"), "--platform", "linux", "--architecture", "arm64", "--output", str(output), "--version", "2026.09.19.0", "--server-url", "https://api.example.test", "--release-url", "https://api.example.test"]
            for value in ["", "http://public@example.invalid/1", "https://public@example.invalid/1\n-X injected=yes", "https://public@example.invalid/1 -X injected=yes", "https://public@example.invalid/1?token=x", "https://public@example.invalid/1'", "https://public@example.invalid/1$(id)"]:
                with self.subTest(value=value):
                    result = subprocess.run(command, env=dict(env,SENTRY_DSN=value), capture_output=True, text=True)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertFalse(args_file.exists())
                    self.assertFalse(output.exists())
            result = subprocess.run(command, env=dict(env,SENTRY_DSN="https://public@example.invalid/1"), capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            args = json.loads(args_file.read_text())
            flags = args[args.index('-ldflags')+1]
            self.assertIn('buildinfo.DefaultSentryDSN=https://public@example.invalid/1', flags)
            self.assertIn('buildinfo.DefaultSentryRelease=paperboat:2026.09.19.0', flags)
            self.assertIn('buildinfo.Distribution=official', flags)
            self.assertIn('buildinfo.DefaultServerURL=https://api.example.test', flags)

if __name__ == "__main__":
    unittest.main()
