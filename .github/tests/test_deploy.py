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
            self.assertEqual(services["migrate"]["depends_on"]["postgres"]["condition"], "service_healthy")
            self.assertEqual(services["postgres"]["ports"][0]["host_ip"], "127.0.0.1")
            self.assertNotIn("POSTGRES_PASSWORD", services["postgres"]["environment"])
            self.assertEqual(services["postgres"]["environment"]["POSTGRES_PASSWORD_FILE"], "/run/secrets/postgres_password")
            secret_mount = next(v for v in services["server"]["volumes"] if v["target"] == "/run/secrets")
            self.assertTrue(secret_mount["read_only"])
            self.assertEqual(services["proxy"]["environment"]["PAPERBOAT_API_HOST"], "api.example.test")


if __name__ == "__main__":
    unittest.main()
