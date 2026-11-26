"""Check the rendered managed stack without starting any service."""
import json
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]


class DeploymentCompositionTest(unittest.TestCase):
    def test_migration_fences_server_and_secrets_stay_private(self):
        docker = ["docker"]
        check = subprocess.run(docker + ["info"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if check.returncode:
            docker = ["sudo", "-n", "docker"]
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "server.env").write_text("PAPERBOAT_ENV=test\n")
            (root / "secrets").mkdir()
            for name in ["postgres_password", "diagnostics_access_key", "diagnostics_secret_key"]:
                (root / "secrets" / name).write_text("fixture-only")
            (root / ".env").write_text("\n".join([
                "PAPERBOAT_POSTGRES_IMAGE=postgres:17", "PAPERBOAT_SERVER_IMAGE=paperboat-server:test",
                "PAPERBOAT_DIAGNOSTICS_IMAGE=minio/minio:test", "PAPERBOAT_PROXY_IMAGE=caddy:2",
                "PAPERBOAT_API_HOST=api.example.test", "PAPERBOAT_DOWNLOAD_HOST=get.example.test",
                "PAPERBOAT_DIAGNOSTICS_HOST=diagnostics.example.test",
            ]) + "\n")
            command = docker + ["compose", "--project-directory", str(root), "--env-file", str(root / ".env"),
                                "-f", str(ROOT / "deploy/compose.yaml"), "config", "--format", "json"]
            result = subprocess.run(command, capture_output=True, text=True, timeout=15, check=True)
            services = json.loads(result.stdout)["services"]
            self.assertEqual(services["server"]["depends_on"]["migrate"]["condition"], "service_completed_successfully")
            # The database runs on its own host (deploy/README.md "Database host"): no Postgres in this stack.
            self.assertNotIn("postgres", services)
            self.assertNotIn("depends_on", services["migrate"])
            secret_mount = next(v for v in services["server"]["volumes"] if v["target"] == "/run/secrets")
            self.assertTrue(secret_mount["read_only"])
            self.assertEqual(services["proxy"]["environment"]["PAPERBOAT_API_HOST"], "api.example.test")
            self.assertNotIn("PAPERBOAT_CONSOLE_HOST", services["proxy"]["environment"])
            release_mount = next(v for v in services["proxy"]["volumes"] if v["target"] == "/srv/paperboat-releases")
            self.assertTrue(release_mount["read_only"])


if __name__ == "__main__":
    unittest.main()
