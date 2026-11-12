"""staging.py: the build's source archive is the repository root of the commit (it was the deploy directory: no server/ to build)."""
import importlib.util
import io
import contextlib
import hashlib
import json
import os
import tempfile
import subprocess
import tarfile
import unittest
from pathlib import Path

spec = importlib.util.spec_from_file_location('staging', Path(__file__).with_name('staging.py'))
staging = importlib.util.module_from_spec(spec)
spec.loader.exec_module(staging)


class SourceArchiveTest(unittest.TestCase):
    def test_archive_holds_every_module_the_image_builds_from_and_not_the_excluded_trees(self):
        head = subprocess.run(['git', 'rev-parse', 'HEAD'], capture_output=True, text=True, check=True, cwd=staging.REPO).stdout.strip()
        short, full, tar = staging.source_archive('HEAD')
        self.assertEqual(full, head)
        self.assertTrue(full.startswith(short))
        names = tarfile.open(fileobj=io.BytesIO(tar)).getnames()
        for required in ('server/deploy/Dockerfile', 'go.work', 'shared', 'cli', 'deploy/staging.py'):
            self.assertIn(required, names)
        self.assertFalse([n for n in names if n.split('/')[0] in ('console', 'marketing', 'tools')])


class BuildVersionTest(unittest.TestCase):
    def test_build_version_matches_the_dockerfile_pattern(self):
        import re
        calls = []
        original = (staging.subprocess.run, staging.ssh, staging.source_archive)
        staging.source_archive = lambda commit: ('abcdef0', 'abcdef0123456789abcdef0123456789abcdef01', b'')
        staging.subprocess.run = lambda *a, **k: type('Done', (), {'returncode': 0, 'stderr': b''})()
        staging.ssh = lambda host, script, **k: calls.append(script)
        try:
            staging.build('HEAD')
        finally:
            staging.subprocess.run, staging.ssh, staging.source_archive = original
        version = re.search(r'PAPERBOAT_VERSION=(\S+)', calls[0]).group(1)
        self.assertRegex(version, r'^[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.(0|[1-9][0-9]*)$')  # server/deploy/Dockerfile's check


class SeedAuthorityTest(unittest.TestCase):
    def test_session_is_private_random_persistent_and_only_hashes_reach_database(self):
        original = staging.remote, staging.sql
        authorities = []
        try:
            for _ in range(2):
                with tempfile.TemporaryDirectory() as root:
                    Path(root, 'secrets').mkdir()
                    statements = []
                    def remote(script, **kwargs):
                        return subprocess.check_output(['sh', '-c', script], cwd=root, text=True)
                    staging.remote, staging.sql = remote, statements.append
                    with contextlib.redirect_stdout(io.StringIO()):
                        staging.seed()
                        staging.seed()
                    path = Path(root, '.fixture-auth.json')
                    self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                    authority = json.loads(path.read_text())
                    authorities.append(authority)
                    self.assertEqual(staging.fixture_credentials(), authority)
                    sql = '\n'.join(statements)
                    for key, value in authority.items():
                        self.assertGreaterEqual(len(value), 48)
                        self.assertNotIn(value, sql)
                        self.assertIn(hashlib.sha256(value.encode()).hexdigest(), sql)
            self.assertNotEqual(authorities[0], authorities[1])
        finally:
            staging.remote, staging.sql = original


if __name__ == '__main__':
    unittest.main()


class RegistrationTest(unittest.TestCase):
    def register(self, function, image):
        from unittest.mock import patch
        statements, commands = [], []
        def remote(command, stdin=None, **kwargs):
            commands.append(command)
            return '{"keys":[]}'
        with patch.object(staging, 'remote', remote), patch.object(staging, 'sql', statements.append), \
                patch.object(staging, 'read_remote_env', lambda *_: {'PAPERBOAT_ENCRYPTION_KEY': 'test-key'}), \
                patch.object(staging, 'domain', lambda: 'stg.example.test'), patch.object(staging, 'api_url', lambda: 'https://anvit.stg.example.test:18443'):
            function(image)
        return statements, commands

    def test_carrier_certificate_pin_is_the_public_key_hash_of_an_ed25519_certificate_for_the_host(self):
        from cryptography import x509
        from cryptography.hazmat.primitives import serialization
        from cryptography.hazmat.primitives.asymmetric import ed25519
        cert_pem, key_pem, pin = staging.carrier_certificate('anvit.stg.example.test')
        cert = x509.load_pem_x509_certificate(cert_pem.encode())
        spki = cert.public_key().public_bytes(serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo)
        self.assertEqual(pin, hashlib.sha256(spki).hexdigest())
        self.assertIsInstance(cert.public_key(), ed25519.Ed25519PublicKey)
        self.assertEqual(cert.extensions.get_extension_for_class(x509.SubjectAlternativeName).value.get_values_for_type(x509.DNSName), ['anvit.stg.example.test'])

    def test_relay_is_registered_as_a_relay_on_a_public_range_tcp_port_and_replaces_only_its_own_install(self):
        statements, commands = self.register(staging.relay, 'paperboat-relay:stg-test')
        joined = '\n'.join(statements)
        self.assertIn("scope_id='usr_staging' AND capability='relay'", statements[0])
        self.assertIn("ARRAY['relay'],ARRAY['derp_quic','derp_wss']", joined)
        self.assertIn(f"'anvit.stg.example.test',{staging.RELAY_TCP_PORT},{staging.RELAY_QUIC_PORT}", joined)
        self.assertIn("'usr_staging','relay','self-hosted-only'", joined)
        self.assertNotIn('control_usage_verification_keys', joined)
        self.assertTrue(10000 <= staging.RELAY_TCP_PORT <= 59999 and 10000 <= staging.RELAY_QUIC_PORT <= 59999 and staging.RELAY_TCP_PORT != staging.RELAY_QUIC_PORT)
        run = commands[-1]
        self.assertIn(f'-p {staging.RELAY_TCP_PORT}:{staging.RELAY_TCP_PORT} ', run)
        self.assertIn(f'-wss-listen 0.0.0.0:{staging.RELAY_TCP_PORT}', run)
        self.assertIn('-issuer https://anvit.stg.example.test:18443', run)

    def test_edge_replaces_only_its_own_install_so_a_relay_survives_it(self):
        statements, _ = self.register(staging.edge, 'paperboat-tunnel:stg-test')
        self.assertIn("scope_id='usr_staging' AND capability='tunnel'", statements[0])
        self.assertNotIn("capability='relay'", '\n'.join(statements))


class CertificateEnvironmentTest(unittest.TestCase):
    def test_staging_domains_have_a_complete_certificate_runtime_confined_to_staging(self):
        import shlex
        from unittest.mock import patch
        prod = {"PAPERBOAT_CERTIFICATES_DNS_ZONE_ID": "test-zone", staging.DNS_ENV: "dns-test-token"}
        existing = {"PAPERBOAT_WORKOS_CLIENT_ID": "client-test", "PAPERBOAT_WORKOS_API_KEY": "workos-test"}
        with (
            patch.object(staging, "api_url", return_value="https://api.stg.example.test"),
            patch.object(staging, "domain", return_value="stg.example.test"),
            patch.object(staging, "new_acme_key", return_value="test-signer"),
        ):
            values = dict(item.split("=", 1) for item in shlex.split(staging.build_env(existing, prod)))
            self.assertEqual(values["PAPERBOAT_CERTIFICATES_ENABLED"], "true")
            self.assertEqual(values["PAPERBOAT_PREVIEW_BASE_DOMAIN"], values["PAPERBOAT_TUNNEL_BASE_DOMAIN"])
            self.assertNotEqual(values["PAPERBOAT_PREVIEW_BASE_DOMAIN"], values["PAPERBOAT_RUNTIME_BASE_DOMAIN"])
            self.assertNotEqual(values["PAPERBOAT_PREVIEW_BASE_DOMAIN"], "api.stg.example.test")
            self.assertEqual(values["PAPERBOAT_CERTIFICATES_CHALLENGE_ZONE"], "acme.stg.example.test")
            self.assertEqual(values["PAPERBOAT_CERTIFICATES_DNS_WRITE_BASE"], "stg.example.test")
            self.assertEqual(values["PAPERBOAT_CERTIFICATES_DNS_ZONE_ID"], "test-zone")
            self.assertEqual(values["PAPERBOAT_CERTIFICATES_PUBLIC_DNS_ZONE_ID"], "test-zone")
            self.assertEqual(values[staging.CERT_DISTRIBUTION_ENV], values["PAPERBOAT_EDGE_CONTROL_CREDENTIAL"])
            self.assertGreaterEqual(len(values[staging.CERT_MASTER_ENV]), 32)
            again = dict(item.split("=", 1) for item in shlex.split(staging.build_env(values, prod)))
            self.assertEqual(again[staging.CERT_MASTER_ENV], values[staging.CERT_MASTER_ENV])
            self.assertEqual(again[staging.CERT_DISTRIBUTION_ENV], values[staging.CERT_DISTRIBUTION_ENV])
