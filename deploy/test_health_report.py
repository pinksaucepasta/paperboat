"""push() logs in as the operations operator (password, then the authenticator code) and posts through the operator API
with the operator confirmation headers; without configuration it sends nothing, and a failure never raises."""
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).parent))
import health_report  # noqa: E402


class Response:
    def __init__(self, body):
        self.status, self.body = 200, json.dumps(body).encode()

    def read(self):
        return self.body

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False


class PushTest(unittest.TestCase):
    def test_operator_login_then_report(self):
        sent = []

        def open_(request, timeout):
            sent.append(request)
            if request.full_url.endswith("/v1/operator/login"):
                return Response({"data": {"csrf_token": "csrf", "mfa": "verify"}})
            return Response({"data": {"status": "ok"}})

        with tempfile.TemporaryDirectory() as d:
            cred = Path(d) / "operator"
            cred.write_text("ops@example.test\npw\nJBSWY3DPEHPK3PXP\n")
            env = {"PAPERBOAT_HEALTH_REPORT_URL": "https://api.example.test/", "PAPERBOAT_HEALTH_REPORT_OPERATOR_FILE": str(cred)}
            with mock.patch.dict(os.environ, env, clear=True), \
                    mock.patch.object(health_report.urllib.request.OpenerDirector, "open", lambda self, r, timeout: open_(r, timeout)):
                health_report.push("deploy", {"rolled_back": True})
        self.assertEqual([r.full_url for r in sent], ["https://api.example.test/v1/operator/login",
                                                      "https://api.example.test/v1/operator/mfa/verify",
                                                      "https://api.example.test/v1/admin/host-health/reports"])
        self.assertEqual(json.loads(sent[0].data), {"email": "ops@example.test", "password": "pw"})
        self.assertRegex(json.loads(sent[1].data)["code"], r"^\d{6}$")
        report = sent[2]
        self.assertEqual(json.loads(report.data), {"source": "deploy", "payload": {"rolled_back": True}})
        self.assertEqual(report.get_header("X-csrf-token"), "csrf")
        self.assertEqual(report.get_header("Operator-target"), "deploy")
        self.assertEqual(report.get_header("Operator-reason"), "host-health-report")
        self.assertGreaterEqual(len(report.get_header("Idempotency-key")), 8)
        self.assertIsNone(report.get_header("Authorization"))

    def test_off_without_config_and_never_raises(self):
        with mock.patch.dict(os.environ, {}, clear=True), mock.patch.object(health_report.Operator, "login") as login:
            health_report.push("hosts", {})
        login.assert_not_called()
        env = {"PAPERBOAT_HEALTH_REPORT_URL": "https://127.0.0.1:1", "PAPERBOAT_HEALTH_REPORT_OPERATOR_FILE": "/nonexistent"}
        with mock.patch.dict(os.environ, env, clear=True):
            health_report.push("hosts", {})
        # The password is never sent to a plaintext origin.
        env["PAPERBOAT_HEALTH_REPORT_URL"] = "http://api.example.test"
        with mock.patch.dict(os.environ, env, clear=True), mock.patch.object(health_report.Operator, "login") as login:
            health_report.push("hosts", {})
        login.assert_not_called()

    def test_totp_matches_rfc6238(self):
        self.assertEqual(health_report.totp("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", now=59), "287082")


if __name__ == "__main__":
    unittest.main()
