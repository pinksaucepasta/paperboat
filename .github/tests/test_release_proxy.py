"""Exercise configured download-host routing against a real isolated Caddy."""
import os
from pathlib import Path
import subprocess
import tempfile
import time
import unittest
import uuid

ROOT = Path(__file__).resolve().parents[2]

@unittest.skipUnless(os.environ.get("PAPERBOAT_TEST_CADDY_IMAGE"), "configured Caddy integration image required")
class ReleaseProxyRoutingTest(unittest.TestCase):
    def test_targets_are_public_only_on_download_host(self):
        docker = ["docker"]
        if subprocess.run(docker + ["info"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode:
            docker = ["sudo", "-n", "docker"]
        name = "paperboat-release-probe-" + uuid.uuid4().hex
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            releases = root / "releases"
            targets = releases / "tuf" / "targets"
            targets.mkdir(parents=True)
            payload = b"public-artifact-fixture"
            (targets / "fixture.bin").write_bytes(payload)
            (releases / "protected-marker.txt").write_bytes(b"outside-target-directory")
            config = root / "Caddyfile"
            config.write_bytes((ROOT / "deploy/Caddyfile").read_bytes())
            command = docker + ["run", "-d", "--name", name, "--network", "none", "--read-only",
                "--tmpfs", "/data", "--tmpfs", "/config",
                "--mount", f"type=bind,src={config},dst=/etc/caddy/Caddyfile,readonly",
                "--mount", f"type=bind,src={releases},dst=/srv/paperboat-releases,readonly"]
            for key, host in [("API", "api"), ("DOWNLOAD", "get"), ("DIAGNOSTICS", "diagnostics")]:
                command += ["-e", f"PAPERBOAT_{key}_HOST=http://{host}.example.test:8080"]
            command += [os.environ["PAPERBOAT_TEST_CADDY_IMAGE"]]
            try:
                subprocess.run(command, capture_output=True, check=True, timeout=30)
                def get(host, path):
                    return subprocess.run(docker + ["exec", name, "wget", "-qO-",
                        "--header", f"Host: {host}.example.test:8080", "http://127.0.0.1:8080" + path],
                        capture_output=True, timeout=10)
                deadline = time.monotonic() + 15
                while True:
                    response = get("get", "/tuf/targets/fixture.bin")
                    if response.returncode == 0 or time.monotonic() >= deadline:
                        break
                    time.sleep(0.1)
                self.assertEqual(response.returncode, 0, "download host did not serve its target")
                self.assertEqual(response.stdout, payload)
                response = get("api", "/tuf/targets/fixture.bin")
                self.assertNotEqual(response.returncode, 0, "API host served a CLI binary")
                self.assertNotEqual(response.stdout, payload)
                response = get("get", "/protected-marker.txt")
                self.assertNotEqual(response.returncode, 0, "download host exposed another release-root file")
                self.assertNotEqual(response.stdout, b"outside-target-directory")
            finally:
                subprocess.run(docker + ["rm", "-f", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15)

if __name__ == "__main__":
    unittest.main()
