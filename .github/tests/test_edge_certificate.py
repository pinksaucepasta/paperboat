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
                (root / "edge-running").touch()
                if current:
                    cert = state / "public-acme/live/paperboat-edge-ip/fullchain.pem"
                    cert.parent.mkdir(parents=True)
                    cert.write_text("fixture certificate")
                binaries = root / "bin"
                binaries.mkdir()
                executable = '''#!/usr/bin/python3
import os,pathlib,sys,json
root=pathlib.Path(os.environ['CERTIFICATE_FIXTURE'])
name=pathlib.Path(sys.argv[0]).name
with (root/'events').open('a') as out: out.write(name+' '+sys.argv[1]+'\\n')
if name=='systemctl': (root/'active').write_text('yes' if sys.argv[1]=='start' else 'no')
if name=='docker' and sys.argv[1]=='ps':
 print('owned-edge'); sys.exit(0)
if name=='docker' and sys.argv[1]=='inspect':
 state=str(root/'installation')
 print(json.dumps([{'Id':'owned-edge','Config':{'Labels':{'io.paperboat.pbh-owner':'fixture'},'Cmd':['run','--state-dir',state+'/tunnel']},'Mounts':[{'Type':'bind','Source':state,'Destination':state}]}])); sys.exit(0)
if name=='docker' and sys.argv[1]=='stop':
 (root/'edge-running').unlink(); sys.exit(0)
if name=='docker':
 if (root/'edge-running').exists(): sys.exit(26)
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
                            ["systemctl stop", "docker ps", "docker inspect", "docker stop", "docker run"] + ([] if failure else ["pbh configure-tls"]) + ["systemctl start"])
                self.assertEqual(events, expected)
                if failure:
                    self.assertIn("issue public IP certificate failed", result.stdout)
                    self.assertFalse((root / "issued").exists())


    def test_subject_flags_cover_ip_and_dns_names(self):
        import importlib.util
        spec = importlib.util.spec_from_file_location("edge_certificate", ROOT / "deploy/edge-ip-certificate.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        self.assertEqual(module.subject_flags("93.184.216.34"), ["--ip-address", "93.184.216.34"])
        self.assertEqual(module.subject_flags("edge-1.example.test"), ["--domain", "edge-1.example.test"])
        for refused in ["10.0.0.1", "127.0.0.1", "bad host", "-bad.example.test", "a..b", "x" * 254]:
            with self.assertRaises(ValueError, msg=refused):
                module.subject_flags(refused)

    def test_http01_address_binds_the_edge_listen_host_not_the_wildcard(self):
        # prod 2026-11-13: the machine guard holds 127.100.0.1:80, so certbot's wildcard bind failed on oracle.
        import importlib.util
        spec = importlib.util.spec_from_file_location("edge_certificate", ROOT / "deploy/edge-ip-certificate.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        for listen, flags in [("10.0.0.221:8443", ["--http-01-address", "10.0.0.221"]),
                              ("[2001:db8::1]:8443", ["--http-01-address", "2001:db8::1"]),
                              ("0.0.0.0:8443", []), ("[::]:8443", [])]:
            with self.subTest(listen=listen), tempfile.TemporaryDirectory() as directory:
                state = Path(directory)
                (state / "selfhost.json").write_text(json.dumps({"config": {"listen": listen}}))
                self.assertEqual(module.http01_address(state), flags)
        with tempfile.TemporaryDirectory() as directory:
            self.assertEqual(module.http01_address(Path(directory)), [])

    def test_timer_retries_a_failed_renewal_inside_the_window(self):
        # The renewal window is two days; a collision with a service restart must leave many retries.
        timer = (ROOT / "deploy/paperboat-edge-certificate.timer").read_text()
        self.assertIn("OnCalendar=*-*-* 00/6:00:00", timer)


if __name__ == "__main__":
    unittest.main()
