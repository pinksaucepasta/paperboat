"""Exercise certificate renewal's service lifecycle through real subprocesses."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


class EdgeCertificateLifecycleTest(unittest.TestCase):
    def test_renewal_restores_service_on_success_and_issuance_failure(self):
        for failure, current in [(False, False), (True, False), (False, True)]:
            with self.subTest(failure=failure, current=current), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                state = root / "installation"
                (state / "tunnel").mkdir(parents=True)
                (state / "tunnel/deployment.json").write_text(json.dumps({"connector_advertise_host": "93.184.216.34"}))
                (root / "active").write_text("yes")
                if current:
                    cert = state / "public-acme/live/paperboat-edge-ip/fullchain.pem"
                    cert.parent.mkdir(parents=True)
                    cert.write_text("fixture certificate")
                binaries = root / "bin"
                binaries.mkdir()
                executable = '''#!/usr/bin/python3
import os,pathlib,sys
root=pathlib.Path(os.environ['CERTIFICATE_FIXTURE'])
name=pathlib.Path(sys.argv[0]).name
with (root/'events').open('a') as out: out.write(name+' '+sys.argv[1]+'\\n')
if name=='systemctl': (root/'active').write_text('yes' if sys.argv[1]=='start' else 'no')
if name=='docker':
 if os.environ['CERTIFICATE_FAIL']=='1': sys.exit(23)
 (root/'issued').touch()
if name=='pbh' and not (root/'issued').exists(): sys.exit(24)
'''
                for name in ["docker", "systemctl", "pbh", "openssl"]:
                    path = binaries / name
                    path.write_text(executable)
                    path.chmod(0o755)
                environment = dict(os.environ, CERTIFICATE_FIXTURE=str(root),
                                   CERTIFICATE_FAIL="1" if failure else "0",
                                   PATH=str(binaries) + ":" + os.environ["PATH"])
                result = subprocess.run([sys.executable, str(ROOT / "deploy/edge-ip-certificate.py"),
                                         "--state-dir", str(state), "--pbh", str(binaries / "pbh"),
                                         "--email", "operator@example.test"], env=environment,
                                        capture_output=True, text=True, timeout=10)
                self.assertEqual(result.returncode, int(failure), result.stdout + result.stderr)
                self.assertEqual((root / "active").read_text(), "yes")
                events = (root / "events").read_text().splitlines()
                expected = (["openssl x509"] if current else
                            ["systemctl stop", "docker run"] + ([] if failure else ["pbh configure-tls"]) + ["systemctl start"])
                self.assertEqual(events, expected)
                if failure:
                    self.assertIn("issue public IP certificate failed", result.stdout)
                    self.assertFalse((root / "issued").exists())


if __name__ == "__main__":
    unittest.main()
