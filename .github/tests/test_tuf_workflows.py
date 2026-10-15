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
    def assert_empty_origin_preflight(self, directory, publication):
        origin = directory / "origin"
        (origin / "current").mkdir(parents=True)
        (origin / "staging").mkdir()
        binaries = directory / "preflight-bin"
        binaries.mkdir()
        ssh = binaries / "ssh"
        ssh.write_text("#!/usr/bin/env python3\nimport os,sys,subprocess\n"
                       "body=sys.stdin.read().replace('/opt/paperboat/releases',os.environ['FIXTURE_ORIGIN'])\n"
                       "sys.exit(subprocess.run(['bash','-c',body]).returncode)\n")
        ssh.chmod(0o755)
        curl = binaries / "curl"
        curl.write_text("#!/bin/sh\nexit 77\n")
        curl.chmod(0o755)
        step = next(s for s in publication["jobs"]["release-authority"]["steps"]
                    if s.get("name") == "Verify production metadata origin is readable")
        environment = dict(os.environ, FIXTURE_ORIGIN=str(origin), RELEASE_HOST="origin.example.test",
                           RELEASE_URL="https://get.example.test/tuf", RUNNER_TEMP=str(directory),
                           PATH=str(binaries)+":"+os.environ["PATH"])
        result = subprocess.run(["bash", "-c", step["run"]], env=environment,
                                capture_output=True, timeout=20)
        self.assertEqual(result.returncode, 0, "empty protected origin cannot qualify its first release")
        (origin / "current/tuf").mkdir()
        result = subprocess.run(["bash", "-c", step["run"]], env=environment,
                                capture_output=True, timeout=20)
        self.assertNotEqual(result.returncode, 0, "partial TUF origin was treated as a fresh installation")

    def assert_empty_origin_initialization(self, directory, publication, root, secret):
        workspace = directory / "checkout"
        trusted = workspace / "cli/internal/hostruntime/bootstrap/trusted-root.json"
        trusted.parent.mkdir(parents=True)
        trusted.write_bytes(root.read_bytes())
        binaries = directory / "bin"
        binaries.mkdir()
        ssh = binaries / "ssh"
        step = next(s for s in publication["jobs"]["release-publication"]["steps"]
                    if s.get("name", "").startswith("Read and validate current production TUF"))
        for status, expected in [(42, 0), (255, 255)]:
            with self.subTest(origin_status=status):
                ssh.write_text(f"#!/bin/sh\nexit {status}\n")
                ssh.chmod(0o755)
                runner = directory / f"runner-{status}"
                runner.mkdir()
                environment = dict(os.environ, PAPERBOAT_TUF_CI="1", PAPERBOAT_TUF_ONLINE_KEY=secret,
                                   GITHUB_WORKSPACE=str(workspace), RUNNER_TEMP=str(runner),
                                   RELEASE_HOST="origin.example.test", PATH=str(binaries)+":"+os.environ["PATH"])
                result = subprocess.run(["bash", "-c", step["run"]], cwd=ROOT / "cli",
                                        env=environment, capture_output=True, timeout=60)
                self.assertEqual(result.returncode, expected,
                                 "first publication did not distinguish absent origin from SSH failure")
                metadata = runner / "tuf-candidate/metadata/root.json"
                self.assertEqual(metadata.is_file(), status == 42)

    def test_release_creation_uses_source_repository_and_verified_tag(self):
        publication = yaml.safe_load((ROOT / ".github/workflows/release.yml").read_text())
        step = next(s for s in publication["jobs"]["release-publication"]["steps"]
                    if s.get("name") == "Create or resume the immutable GitHub release")
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            tag, source, repository = "2026.10.15.0", "a" * 40, "example/source"
            configured = publication["env"]["PAPERBOAT_GITHUB_REPOSITORY"]
            configured = configured.replace("${{ github.repository }}", repository)
            git = directory / "git"
            git.write_text(f"#!/bin/sh\nprintf '%s\\t%s\\n' '{source}' 'refs/tags/{tag}'\n")
            git.chmod(0o755)
            gh = directory / "gh"
            gh.write_text("#!/usr/bin/env python3\nimport json,os,sys\n"
                          "with open(os.environ['GH_CALLS'],'a') as f: f.write(json.dumps(sys.argv[1:])+'\\n')\n"
                          "sys.exit(1 if sys.argv[1:3]==['release','view'] else 0)\n")
            gh.chmod(0o755)
            calls = directory / "calls.jsonl"
            environment = dict(os.environ, PATH=str(directory)+":"+os.environ["PATH"],
                               RELEASE_VERSION=tag, RELEASE_SOURCE_SHA=source,
                               GITHUB_REPOSITORY=repository, PAPERBOAT_GITHUB_REPOSITORY=configured,
                               GH_CALLS=str(calls))
            result = subprocess.run(["bash", "-c", step["run"]], env=environment,
                                    capture_output=True, timeout=10)
            self.assertEqual(result.returncode, 0, "qualified immutable tag cannot create its release")
            recorded = [json.loads(line) for line in calls.read_text().splitlines()]
            creation = next(args for args in recorded if args[:2] == ["release", "create"])
            self.assertEqual(creation[creation.index("--repo")+1], repository)
            self.assertIn("--verify-tag", creation)

    def test_launch_candidate_accepts_three_binaries_and_rejects_incomplete_or_extra_products(self):
        publication = yaml.safe_load((ROOT / ".github/workflows/release.yml").read_text())
        step = next(s for s in publication["jobs"]["release-publication"]["steps"]
                    if s.get("name", "").startswith("Validate the exact"))
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            artifacts = directory / "candidate-artifacts"
            artifacts.mkdir()
            products = ["pb-linux-amd64", "pb-linux-arm64", "pb-darwin-arm64"]
            for name in products:
                (artifacts / name).write_bytes(b"qualified product bytes")
            environment = dict(os.environ, RUNNER_TEMP=str(directory))
            def validate():
                return subprocess.run(["bash", "-c", step["run"]], env=environment,
                                      capture_output=True, timeout=10).returncode
            self.assertEqual(validate(), 0, "launch binaries cannot reach signing")
            (artifacts / products[0]).unlink()
            self.assertNotEqual(validate(), 0, "missing launch product accepted")
            (artifacts / products[0]).write_bytes(b"qualified product bytes")
            extra = artifacts / "pb-windows-amd64.exe"
            extra.write_bytes(b"post-launch product")
            self.assertNotEqual(validate(), 0, "unqualified extra product accepted")
            extra.unlink()
            (artifacts / products[1]).write_bytes(b"")
            self.assertNotEqual(validate(), 0, "empty product accepted")

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
            self.assert_empty_origin_initialization(directory, publication, root, secret)
            self.assert_empty_origin_preflight(directory, publication)


if __name__ == "__main__":
    unittest.main()
