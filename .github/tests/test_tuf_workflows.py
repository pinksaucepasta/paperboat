"""Verify workflow secret wiring against the actual single-key TUF publisher."""
import base64
import hashlib
import json
from pathlib import Path
import os
import subprocess
import tempfile
import unittest

from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
import yaml

ROOT = Path(__file__).resolve().parents[2]


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode()


class TUFWorkflowSignerTest(unittest.TestCase):
    def test_publication_and_refresh_authorize_the_configured_online_secret(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            tool = directory / "publisher"
            subprocess.run(["go", "build", "-o", str(tool), "./cli/tools/tuf-repository"],
                           cwd=ROOT, capture_output=True, check=True, timeout=120)
            offline, online = Ed25519PrivateKey.generate(), Ed25519PrivateKey.generate()
            keys = {}
            ids = []
            for private in [offline, online]:
                public = private.public_key().public_bytes(serialization.Encoding.Raw,
                                                          serialization.PublicFormat.Raw)
                key = {"keytype": "ed25519", "scheme": "ed25519", "keyval": {"public": public.hex()}}
                identity = hashlib.sha256(canonical(key)).hexdigest()
                keys[identity] = key
                ids.append(identity)
            signed = {"_type": "root", "spec_version": "1.0.31", "version": 1,
                      "expires": "2099-01-01T00:00:00Z", "consistent_snapshot": True,
                      "keys": keys, "roles": {role: {"keyids": [ids[0 if role == "root" else 1]],
                                                      "threshold": 1}
                                              for role in ["root", "targets", "snapshot", "timestamp"]}}
            root = directory / "root.json"
            root.write_bytes(canonical({"signed": signed, "signatures": [
                {"keyid": ids[0], "sig": offline.sign(canonical(signed)).hex()}]}))
            seed = online.private_bytes(serialization.Encoding.Raw, serialization.PrivateFormat.Raw,
                                        serialization.NoEncryption())
            secret = base64.b64encode(seed).decode().rstrip("=")
            publication = yaml.safe_load((ROOT / ".github/workflows/release.yml").read_text())
            refresh = yaml.safe_load((ROOT / ".github/workflows/tuf-refresh.yml").read_text())
            job = publication["jobs"]["release-publication"]
            contexts = [("publication", job.get("env", {}))]
            job = refresh["jobs"]["refresh"]
            for step in job["steps"]:
                if "go run ./tools/tuf-repository" in step.get("run", ""):
                    contexts.append((step["name"], job.get("env", {}) | step.get("env", {})))
            for index, (name, configured) in enumerate(contexts):
                with self.subTest(workflow=name):
                    environment = {k: v for k, v in os.environ.items()
                                   if not k.startswith("PAPERBOAT_TUF_")}
                    for key, value in configured.items():
                        if value == "${{ secrets.PAPERBOAT_TUF_ONLINE_KEY }}":
                            environment[key] = secret
                        elif isinstance(value, str) and "${{" not in value:
                            environment[key] = value
                    repository = directory / str(index)
                    result = subprocess.run([str(tool), "init", "-repository", str(repository),
                                             "-trusted-root", str(root)], env=environment,
                                            capture_output=True, timeout=20)
                    self.assertEqual(result.returncode, 0,
                                     "workflow cannot initialize with its authorized online secret")
                    result = subprocess.run([str(tool), "validate-signers", "-repository", str(repository),
                                             "-trusted-root", str(root)], env=environment,
                                            capture_output=True, timeout=20)
                    self.assertEqual(result.returncode, 0, "workflow signer does not match offline root")


if __name__ == "__main__":
    unittest.main()
